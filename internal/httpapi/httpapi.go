// Package httpapi serves the redirect hot path and the management API.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MahyarNemati/url-shortener-at-scale/internal/analytics"
	"github.com/MahyarNemati/url-shortener-at-scale/internal/cache"
	"github.com/MahyarNemati/url-shortener-at-scale/internal/shortid"
	"github.com/MahyarNemati/url-shortener-at-scale/internal/store"
)

// Deps are the server's collaborators.
type Deps struct {
	Store      store.Store
	Cache      *cache.Cache
	Generator  *shortid.Generator
	Obfuscator *shortid.Obfuscator
	Analytics  *analytics.Recorder
	Log        *slog.Logger
	// BaseURL is the public origin, used to build the returned short URL.
	BaseURL string
	// Token guards the management API. Empty disables writes.
	Token string
	// TrustProxy controls whether X-Forwarded-For is believed.
	TrustProxy bool
	// RedirectStatus is 301 or 302.
	RedirectStatus int
	Now            func() time.Time
	// ReservedSlugs are paths the redirect handler must never claim.
	ReservedSlugs map[string]bool
}

// Server serves HTTP.
type Server struct {
	deps Deps
	// counters feed /metrics without pulling in a metrics library for five
	// numbers.
	redirects, notFound, gone, expired, created atomic.Uint64
}

// New builds a Server.
func New(d Deps) (*Server, error) {
	switch {
	case d.Store == nil:
		return nil, errors.New("httpapi: store is required")
	case d.Cache == nil:
		return nil, errors.New("httpapi: cache is required")
	case d.Generator == nil:
		return nil, errors.New("httpapi: generator is required")
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.RedirectStatus == 0 {
		// 302 by default. A 301 is cached by browsers essentially forever,
		// which means a link can never be retargeted or retired -- a
		// surprising thing to discover after a campaign ends.
		d.RedirectStatus = http.StatusFound
	}
	if d.ReservedSlugs == nil {
		d.ReservedSlugs = defaultReserved()
	}
	return &Server{deps: d}, nil
}

func defaultReserved() map[string]bool {
	// Anything the service itself serves, plus the paths browsers and
	// crawlers request unprompted. Without this, a link could shadow
	// /favicon.ico and the API would start redirecting.
	return map[string]bool{
		"": true, "api": true, "healthz": true, "readyz": true, "metrics": true,
		"favicon.ico": true, "robots.txt": true, "sitemap.xml": true,
		".well-known": true, "static": true, "admin": true,
	}
}

// Handler returns the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		// Short links should not be indexed: the destination should be.
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /\n"))
	})

	mux.Handle("POST /api/v1/links", s.authed(http.HandlerFunc(s.handleCreate)))
	mux.Handle("GET /api/v1/links", s.authed(http.HandlerFunc(s.handleList)))
	mux.Handle("GET /api/v1/links/{slug}", s.authed(http.HandlerFunc(s.handleGetLink)))
	mux.Handle("DELETE /api/v1/links/{slug}", s.authed(http.HandlerFunc(s.handleDelete)))

	// The redirect is the catch-all. Registered without a method: Go's
	// ServeMux rejects "HEAD /{slug}" as conflicting with "GET /healthz"
	// (fewer methods but a more general path), so the method check moves
	// into the handler.
	mux.HandleFunc("/{slug}", s.handleRedirect)

	return mux
}

// ---------- redirect hot path ----------

func (s *Server) handleRedirect(w http.ResponseWriter, r *http.Request) {
	// Only safe methods resolve a link. A POST to a short URL is a mistake,
	// and answering 405 says so rather than silently redirecting and losing
	// the body.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		s.fail(w, r, http.StatusMethodNotAllowed, "only GET and HEAD resolve a short link")
		return
	}

	slug := shortid.Normalize(r.PathValue("slug"))
	now := s.deps.Now()

	if slug == "" || s.deps.ReservedSlugs[strings.ToLower(slug)] {
		s.notFound.Add(1)
		http.NotFound(w, r)
		return
	}

	link, err := s.deps.Cache.Get(r.Context(), slug)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.notFound.Add(1)
		s.record(r, slug, "", http.StatusNotFound, now)
		s.fail(w, r, http.StatusNotFound, "no such link")
		return
	case err != nil:
		s.deps.Log.Error("resolve", "slug", slug, "err", err)
		s.fail(w, r, http.StatusBadGateway, "could not resolve link")
		return
	}

	if link.Deleted {
		// 410 rather than 404: the caller learns the difference between
		// "never existed" and "retired", which matters for a crawler
		// deciding whether to come back.
		s.gone.Add(1)
		s.record(r, slug, link.Target, http.StatusGone, now)
		s.fail(w, r, http.StatusGone, "this link has been deleted")
		return
	}
	if link.Expired(now) {
		s.expired.Add(1)
		s.record(r, slug, link.Target, http.StatusGone, now)
		s.fail(w, r, http.StatusGone, "this link has expired")
		return
	}

	s.redirects.Add(1)
	s.record(r, slug, link.Target, s.deps.RedirectStatus, now)

	// Never let a redirect be cached by an intermediary: the destination can
	// change and the link can be retired, and a cached 302 in a corporate
	// proxy is unfixable from here.
	w.Header().Set("Cache-Control", "private, no-store, max-age=0")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.Redirect(w, r, link.Target, s.deps.RedirectStatus)
}

// record enqueues a click. Non-blocking by construction; see the analytics
// package for why dropping beats blocking.
func (s *Server) record(r *http.Request, slug, target string, status int, now time.Time) {
	if s.deps.Analytics == nil {
		return
	}
	s.deps.Analytics.Record(analytics.FromRequest(r, slug, target, status, now, s.deps.TrustProxy))
}

// fail answers an error, in HTML for a browser and JSON otherwise.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, code int, msg string) {
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(code)
		fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8">`+
			`<title>%d</title><style>body{font:16px/1.5 system-ui;display:grid;`+
			`place-items:center;min-height:100vh;margin:0;color:#15181d;background:#f6f7f9}`+
			`main{text-align:center}h1{font-size:3rem;margin:0}p{color:#6b7280}</style></head>`+
			`<body><main><h1>%d</h1><p>%s</p></main></body></html>`, code, code, msg)
		return
	}
	writeJSON(w, code, map[string]string{"error": msg})
}

// ---------- management API ----------

func (s *Server) authed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Token == "" {
			writeError(w, http.StatusForbidden, "the management API is disabled: no token configured")
			return
		}
		supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(supplied), []byte(s.deps.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="shortener"`)
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type createRequest struct {
	Target string `json:"target"`
	// Slug requests a specific vanity slug.
	Slug string `json:"slug,omitempty"`
	// TTL is a duration string such as "720h"; empty means no expiry.
	TTL   string   `json:"ttl,omitempty"`
	Tags  []string `json:"tags,omitempty"`
	Owner string   `json:"owner,omitempty"`
}

type createResponse struct {
	Slug     string      `json:"slug"`
	ShortURL string      `json:"short_url"`
	Link     *store.Link `json:"link"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !decode(w, r, &req) {
		return
	}

	target, err := validateTarget(req.Target)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	now := s.deps.Now()
	link := &store.Link{Target: target, CreatedAt: now, Owner: req.Owner, Tags: req.Tags}

	if req.TTL != "" {
		d, err := time.ParseDuration(req.TTL)
		if err != nil || d <= 0 {
			writeError(w, http.StatusBadRequest, "ttl must be a positive duration such as \"720h\"")
			return
		}
		link.ExpiresAt = now.Add(d)
	}

	if req.Slug != "" {
		slug := shortid.Normalize(req.Slug)
		if err := validateVanitySlug(slug, s.deps.ReservedSlugs); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		link.Slug = slug
	} else {
		id, err := s.deps.Generator.Next()
		if err != nil {
			s.deps.Log.Error("generate id", "err", err)
			writeError(w, http.StatusServiceUnavailable, "could not generate an id")
			return
		}
		link.ID = id
		// Obfuscate before encoding, so the slug is not an enumerable
		// counter. With no obfuscator this is the identity.
		link.Slug = shortid.Encode(s.deps.Obfuscator.Obfuscate(id))
	}

	if err := s.deps.Store.Create(r.Context(), link); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "that slug is already taken")
			return
		}
		s.deps.Log.Error("create link", "err", err)
		writeError(w, http.StatusInternalServerError, "could not create link")
		return
	}

	// Drop any cached negative for this slug, or a vanity link just created
	// would 404 until the negative TTL expired.
	s.deps.Cache.Invalidate(link.Slug)
	s.created.Add(1)

	writeJSON(w, http.StatusCreated, createResponse{
		Slug:     link.Slug,
		ShortURL: strings.TrimRight(s.deps.BaseURL, "/") + "/" + link.Slug,
		Link:     link,
	})
}

func (s *Server) handleGetLink(w http.ResponseWriter, r *http.Request) {
	link, err := s.deps.Store.Get(r.Context(), shortid.Normalize(r.PathValue("slug")))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such link")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load link")
		return
	}
	writeJSON(w, http.StatusOK, link)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 1000")
			return
		}
		limit = n
	}
	links, err := s.deps.Store.List(r.Context(), r.URL.Query().Get("owner"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list links")
		return
	}
	if links == nil {
		links = []*store.Link{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": links})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	slug := shortid.Normalize(r.PathValue("slug"))
	err := s.deps.Store.Delete(r.Context(), slug)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such link")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete link")
		return
	}
	// Without this the link keeps redirecting until the cache TTL expires,
	// which for a link deleted because it points somewhere harmful is not
	// acceptable.
	s.deps.Cache.Invalidate(slug)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.deps.Store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	cs := s.deps.Cache.Stats()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	var b strings.Builder
	metric := func(name, help, typ string, value any) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, typ, name, value)
	}
	metric("shortener_redirects_total", "Successful redirects.", "counter", s.redirects.Load())
	metric("shortener_not_found_total", "Requests for unknown slugs.", "counter", s.notFound.Load())
	metric("shortener_gone_total", "Requests for deleted slugs.", "counter", s.gone.Load())
	metric("shortener_expired_total", "Requests for expired slugs.", "counter", s.expired.Load())
	metric("shortener_links_created_total", "Links created.", "counter", s.created.Load())
	metric("shortener_cache_hits_total", "Cache hits.", "counter", cs.Hits)
	metric("shortener_cache_negative_hits_total", "Cached negative lookups served.", "counter", cs.NegativeHits)
	metric("shortener_cache_misses_total", "Cache misses.", "counter", cs.Misses)
	metric("shortener_cache_stale_served_total", "Stale entries served after a backend error.", "counter", cs.Stale)
	metric("shortener_cache_coalesced_total", "Misses collapsed into an in-flight load.", "counter", cs.Coalesced)
	metric("shortener_cache_evictions_total", "Entries evicted.", "counter", cs.Evictions)
	metric("shortener_cache_entries", "Entries currently cached.", "gauge", s.deps.Cache.Len())
	if rate := cs.HitRate(); rate >= 0 {
		metric("shortener_cache_hit_ratio", "Cache hit ratio.", "gauge", rate)
	}
	if s.deps.Analytics != nil {
		as := s.deps.Analytics.Stats()
		metric("shortener_clicks_enqueued_total", "Click events enqueued.", "counter", as.Enqueued)
		metric("shortener_clicks_written_total", "Click events written.", "counter", as.Written)
		// The one to alert on: a non-zero drop count means the analytics
		// buffer filled and clicks were lost.
		metric("shortener_clicks_dropped_total", "Click events dropped because the buffer was full.", "counter", as.Dropped)
		metric("shortener_clicks_failed_total", "Click events lost to a failed batch write.", "counter", as.Failed)
	}
	_, _ = w.Write([]byte(b.String()))
}

// ---------- validation ----------

// maxTargetLength bounds a destination. 2048 is the practical URL limit in
// browsers; anything longer is almost certainly abuse.
const maxTargetLength = 2048

// validateTarget checks a destination URL.
func validateTarget(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("target is required")
	}
	if len(raw) > maxTargetLength {
		return "", fmt.Errorf("target is longer than %d characters", maxTargetLength)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("target is not a valid URL: %v", err)
	}
	// Scheme allow-list, not a block-list. A shortener that accepts
	// javascript: or data: targets is an XSS vector handed to every site that
	// renders the link, and file: reads from whoever clicks it.
	switch u.Scheme {
	case "http", "https":
	case "":
		return "", errors.New("target must include a scheme, e.g. https://")
	default:
		return "", fmt.Errorf("scheme %q is not allowed; use http or https", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("target must include a host")
	}
	return u.String(), nil
}

// validateVanitySlug checks a user-chosen slug.
func validateVanitySlug(slug string, reserved map[string]bool) error {
	if len(slug) < 3 || len(slug) > 64 {
		return errors.New("a custom slug must be 3 to 64 characters")
	}
	if reserved[strings.ToLower(slug)] {
		return fmt.Errorf("%q is reserved", slug)
	}
	for i := 0; i < len(slug); i++ {
		c := slug[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			return fmt.Errorf("a custom slug may only contain letters, digits, '-' and '_'; found %q", c)
		}
	}
	return nil
}

// ---------- helpers ----------

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
