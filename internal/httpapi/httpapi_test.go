package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MahyarNemati/url-shortener-at-scale/internal/analytics"
	"github.com/MahyarNemati/url-shortener-at-scale/internal/cache"
	"github.com/MahyarNemati/url-shortener-at-scale/internal/httpapi"
	"github.com/MahyarNemati/url-shortener-at-scale/internal/shortid"
	"github.com/MahyarNemati/url-shortener-at-scale/internal/store"
)

const token = "test-token-at-least-16"

type rig struct {
	srv      *httptest.Server
	store    *store.Memory
	sink     *analytics.Discard
	recorder *analytics.Recorder
}

func newRig(t *testing.T, opts ...func(*httpapi.Deps)) *rig {
	t.Helper()
	st := store.NewMemory()
	sink := &analytics.Discard{}
	rec := analytics.NewRecorder(sink, analytics.Options{
		BatchSize: 1, FlushInterval: 10 * time.Millisecond,
	})
	gen, err := shortid.NewGenerator(0)
	if err != nil {
		t.Fatalf("generator: %v", err)
	}

	deps := httpapi.Deps{
		Store:     st,
		Cache:     cache.New(st, cache.Options{TTL: time.Minute}),
		Generator: gen,
		Analytics: rec,
		BaseURL:   "https://s.example",
		Token:     token,
	}
	for _, o := range opts {
		o(&deps)
	}

	srv, err := httpapi.New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = rec.Close()
		_ = st.Close()
	})
	return &rig{srv: ts, store: st, sink: sink, recorder: rec}
}

// client that does not follow redirects, so a 302 is observable.
func (r *rig) client() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (r *rig) do(t *testing.T, method, path, tok string, body any) *http.Response {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, r.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := r.client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (r *rig) create(t *testing.T, body map[string]any) string {
	t.Helper()
	resp := r.do(t, http.MethodPost, "/api/v1/links", token, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create got %d", resp.StatusCode)
	}
	var out struct {
		Slug     string `json:"slug"`
		ShortURL string `json:"short_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Slug
}

// ---------- redirect ----------

func TestRedirect(t *testing.T) {
	r := newRig(t)
	slug := r.create(t, map[string]any{"target": "https://go.dev/doc"})

	resp := r.do(t, http.MethodGet, "/"+slug, "", nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("got %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "https://go.dev/doc" {
		t.Errorf("Location = %q", loc)
	}
	// A cached 302 in a corporate proxy is unfixable from here, so the
	// response must forbid caching.
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if rp := resp.Header.Get("Referrer-Policy"); rp != "no-referrer" {
		t.Errorf("Referrer-Policy = %q", rp)
	}
}

func TestRedirectIs302ByDefaultAnd301WhenAsked(t *testing.T) {
	r := newRig(t)
	slug := r.create(t, map[string]any{"target": "https://go.dev"})
	if got := r.do(t, http.MethodGet, "/"+slug, "", nil).StatusCode; got != http.StatusFound {
		t.Errorf("default redirect = %d, want 302: a 301 is cached forever and the link can never be retargeted", got)
	}

	perm := newRig(t, func(d *httpapi.Deps) { d.RedirectStatus = http.StatusMovedPermanently })
	slug = perm.create(t, map[string]any{"target": "https://go.dev"})
	if got := perm.do(t, http.MethodGet, "/"+slug, "", nil).StatusCode; got != http.StatusMovedPermanently {
		t.Errorf("configured redirect = %d, want 301", got)
	}
}

func TestUnknownSlugIs404(t *testing.T) {
	r := newRig(t)
	if got := r.do(t, http.MethodGet, "/nosuchslug", "", nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("got %d, want 404", got)
	}
}

func TestDeletedSlugIs410NotFound(t *testing.T) {
	r := newRig(t)
	slug := r.create(t, map[string]any{"target": "https://go.dev"})

	if got := r.do(t, http.MethodDelete, "/api/v1/links/"+slug, token, nil).StatusCode; got != http.StatusNoContent {
		t.Fatalf("delete got %d", got)
	}
	// 410 rather than 404 tells a crawler the difference between "never
	// existed" and "retired".
	if got := r.do(t, http.MethodGet, "/"+slug, "", nil).StatusCode; got != http.StatusGone {
		t.Errorf("got %d, want 410", got)
	}
}

func TestDeleteTakesEffectImmediatelyDespiteTheCache(t *testing.T) {
	r := newRig(t)
	slug := r.create(t, map[string]any{"target": "https://go.dev"})

	// Warm the cache.
	if got := r.do(t, http.MethodGet, "/"+slug, "", nil).StatusCode; got != http.StatusFound {
		t.Fatalf("warm got %d", got)
	}
	r.do(t, http.MethodDelete, "/api/v1/links/"+slug, token, nil)

	// A link removed because it points somewhere harmful must stop
	// redirecting now, not when the TTL expires.
	if got := r.do(t, http.MethodGet, "/"+slug, "", nil).StatusCode; got != http.StatusGone {
		t.Errorf("got %d, want 410 immediately after delete", got)
	}
}

func TestExpiredLinkIs410(t *testing.T) {
	r := newRig(t)
	// Create with a tiny TTL, then wait it out.
	slug := r.create(t, map[string]any{"target": "https://go.dev", "ttl": "20ms"})
	time.Sleep(60 * time.Millisecond)
	if got := r.do(t, http.MethodGet, "/"+slug, "", nil).StatusCode; got != http.StatusGone {
		t.Errorf("got %d, want 410 for an expired link", got)
	}
}

func TestReservedPathsAreNotTreatedAsSlugs(t *testing.T) {
	r := newRig(t)
	// Without a reserved list, a link could shadow /favicon.ico and the API
	// would start redirecting.
	for _, path := range []string{"/favicon.ico", "/robots.txt", "/healthz", "/metrics"} {
		resp := r.do(t, http.MethodGet, path, "", nil)
		if resp.StatusCode == http.StatusFound {
			t.Errorf("%s was treated as a slug", path)
		}
	}
}

func TestNonSafeMethodOnASlugIs405(t *testing.T) {
	r := newRig(t)
	slug := r.create(t, map[string]any{"target": "https://go.dev"})
	resp := r.do(t, http.MethodPost, "/"+slug, "", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("got %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow = %q", allow)
	}
}

func TestHeadResolvesWithoutABody(t *testing.T) {
	r := newRig(t)
	slug := r.create(t, map[string]any{"target": "https://go.dev"})
	resp := r.do(t, http.MethodHead, "/"+slug, "", nil)
	if resp.StatusCode != http.StatusFound {
		t.Errorf("HEAD got %d, want 302", resp.StatusCode)
	}
}

// ---------- creation and validation ----------

func TestCreateReturnsAShortURL(t *testing.T) {
	r := newRig(t)
	resp := r.do(t, http.MethodPost, "/api/v1/links", token, map[string]any{"target": "https://go.dev"})
	var out struct {
		Slug     string `json:"slug"`
		ShortURL string `json:"short_url"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if !strings.HasPrefix(out.ShortURL, "https://s.example/") {
		t.Errorf("ShortURL = %q, want the configured base URL", out.ShortURL)
	}
	if !strings.HasSuffix(out.ShortURL, out.Slug) {
		t.Errorf("ShortURL %q does not end with slug %q", out.ShortURL, out.Slug)
	}
}

func TestDangerousSchemesAreRejected(t *testing.T) {
	// A shortener that accepts javascript: hands an XSS vector to every site
	// that renders the link.
	r := newRig(t)
	for _, target := range []string{
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"file:///etc/passwd",
		"ftp://example.com",
		"vbscript:msgbox(1)",
	} {
		resp := r.do(t, http.MethodPost, "/api/v1/links", token, map[string]any{"target": target})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("target %q got %d, want 400", target, resp.StatusCode)
		}
	}
}

func TestTargetMustHaveASchemeAndHost(t *testing.T) {
	r := newRig(t)
	for _, target := range []string{"", "   ", "example.com", "https://", "/relative/path"} {
		resp := r.do(t, http.MethodPost, "/api/v1/links", token, map[string]any{"target": target})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("target %q got %d, want 400", target, resp.StatusCode)
		}
	}
}

func TestOverlongTargetIsRejected(t *testing.T) {
	r := newRig(t)
	long := "https://example.com/" + strings.Repeat("a", 3000)
	resp := r.do(t, http.MethodPost, "/api/v1/links", token, map[string]any{"target": long})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("got %d, want 400", resp.StatusCode)
	}
}

func TestVanitySlug(t *testing.T) {
	r := newRig(t)
	slug := r.create(t, map[string]any{"target": "https://go.dev", "slug": "go-docs"})
	if slug != "go-docs" {
		t.Fatalf("slug = %q, want go-docs", slug)
	}
	if got := r.do(t, http.MethodGet, "/go-docs", "", nil).StatusCode; got != http.StatusFound {
		t.Errorf("got %d, want 302", got)
	}
}

func TestVanitySlugIsValidated(t *testing.T) {
	r := newRig(t)
	for _, slug := range []string{"ab", "has space", "has/slash", "has.dot", strings.Repeat("a", 65), "api", "healthz"} {
		resp := r.do(t, http.MethodPost, "/api/v1/links", token,
			map[string]any{"target": "https://go.dev", "slug": slug})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("slug %q got %d, want 400", slug, resp.StatusCode)
		}
	}
}

func TestDuplicateVanitySlugIsAConflict(t *testing.T) {
	r := newRig(t)
	r.create(t, map[string]any{"target": "https://go.dev", "slug": "taken"})
	resp := r.do(t, http.MethodPost, "/api/v1/links", token,
		map[string]any{"target": "https://example.com", "slug": "taken"})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("got %d, want 409", resp.StatusCode)
	}
}

func TestVanitySlugIsImmediatelyResolvable(t *testing.T) {
	r := newRig(t)
	// Probe it first so a negative is cached, then create it. Without
	// invalidation on create the new link would 404 until the negative TTL
	// expired.
	if got := r.do(t, http.MethodGet, "/brand-new", "", nil).StatusCode; got != http.StatusNotFound {
		t.Fatalf("precondition: got %d", got)
	}
	r.create(t, map[string]any{"target": "https://go.dev", "slug": "brand-new"})
	if got := r.do(t, http.MethodGet, "/brand-new", "", nil).StatusCode; got != http.StatusFound {
		t.Errorf("got %d, want 302 immediately after create", got)
	}
}

func TestSlugsAreNotSequentialWhenObfuscated(t *testing.T) {
	// Without obfuscation a slug is a base62 counter: mint one, decode it,
	// and you can walk every link in the system.
	r := newRig(t, func(d *httpapi.Deps) {
		d.Obfuscator = shortid.NewObfuscator("a-secret")
	})
	a := r.create(t, map[string]any{"target": "https://a.example"})
	b := r.create(t, map[string]any{"target": "https://b.example"})

	ida, err := shortid.Decode(a)
	if err != nil {
		t.Fatalf("decode %q: %v", a, err)
	}
	idb, err := shortid.Decode(b)
	if err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	if idb == ida+1 || ida == idb+1 {
		t.Errorf("consecutive links produced adjacent slugs %q and %q", a, b)
	}
}

func TestInvalidTTLIsRejected(t *testing.T) {
	r := newRig(t)
	for _, ttl := range []string{"soon", "-1h", "0s"} {
		resp := r.do(t, http.MethodPost, "/api/v1/links", token,
			map[string]any{"target": "https://go.dev", "ttl": ttl})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("ttl %q got %d, want 400", ttl, resp.StatusCode)
		}
	}
}

func TestUnknownFieldsAreRejected(t *testing.T) {
	r := newRig(t)
	resp := r.do(t, http.MethodPost, "/api/v1/links", token,
		map[string]any{"target": "https://go.dev", "targte": "typo"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("got %d, want 400 for an unknown field", resp.StatusCode)
	}
}

// ---------- auth ----------

func TestManagementRequiresAToken(t *testing.T) {
	r := newRig(t)
	for _, tok := range []string{"", "wrong"} {
		resp := r.do(t, http.MethodPost, "/api/v1/links", tok, map[string]any{"target": "https://go.dev"})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q got %d, want 401", tok, resp.StatusCode)
		}
	}
}

func TestRedirectNeedsNoToken(t *testing.T) {
	r := newRig(t)
	slug := r.create(t, map[string]any{"target": "https://go.dev"})
	if got := r.do(t, http.MethodGet, "/"+slug, "", nil).StatusCode; got != http.StatusFound {
		t.Errorf("got %d: the redirect must be public", got)
	}
}

func TestNoTokenConfiguredDisablesManagement(t *testing.T) {
	r := newRig(t, func(d *httpapi.Deps) { d.Token = "" })
	resp := r.do(t, http.MethodPost, "/api/v1/links", "anything", map[string]any{"target": "https://go.dev"})
	// Closed, not open: without a token anyone could mint links pointing
	// anywhere, which is a phishing service.
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("got %d, want 403", resp.StatusCode)
	}
}

// ---------- analytics ----------

func TestClicksAreRecordedWithoutBlockingTheRedirect(t *testing.T) {
	r := newRig(t)
	slug := r.create(t, map[string]any{"target": "https://go.dev"})

	for i := 0; i < 20; i++ {
		r.do(t, http.MethodGet, "/"+slug, "", nil)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r.sink.Count() >= 20 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := r.sink.Count(); got < 20 {
		t.Errorf("recorded %d clicks, want at least 20", got)
	}
	if dropped := r.recorder.Stats().Dropped; dropped != 0 {
		t.Errorf("dropped %d events with a healthy sink", dropped)
	}
}

func TestMissesAndGonesAreAlsoCounted(t *testing.T) {
	r := newRig(t)
	r.do(t, http.MethodGet, "/nosuchslug", "", nil)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && r.sink.Count() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	// A 404 is a data point: it is how you discover a broken QR code or a
	// crawler walking your keyspace.
	if r.sink.Count() == 0 {
		t.Error("a 404 should be recorded too")
	}
}

// ---------- metrics ----------

func TestMetricsExposeCacheAndRedirectCounters(t *testing.T) {
	r := newRig(t)
	slug := r.create(t, map[string]any{"target": "https://go.dev"})
	for i := 0; i < 5; i++ {
		r.do(t, http.MethodGet, "/"+slug, "", nil)
	}
	r.do(t, http.MethodGet, "/nope", "", nil)

	resp := r.do(t, http.MethodGet, "/metrics", "", nil)
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	out := string(raw)

	for _, want := range []string{
		"shortener_redirects_total 5",
		"shortener_not_found_total 1",
		"shortener_cache_hits_total",
		"shortener_cache_hit_ratio",
		"shortener_clicks_dropped_total",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
}

func TestHealthAndReadiness(t *testing.T) {
	r := newRig(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		if got := r.do(t, http.MethodGet, path, "", nil).StatusCode; got != http.StatusOK {
			t.Errorf("%s got %d", path, got)
		}
	}
}

func TestRobotsDisallowsIndexing(t *testing.T) {
	r := newRig(t)
	resp := r.do(t, http.MethodGet, "/robots.txt", "", nil)
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read robots.txt: %v", err)
	}
	// Short links should not be indexed; the destination should be.
	if !strings.Contains(string(raw), "Disallow: /") {
		t.Errorf("robots.txt = %q", raw)
	}
}

func TestListLinks(t *testing.T) {
	r := newRig(t)
	r.create(t, map[string]any{"target": "https://a.example", "owner": "alice"})
	r.create(t, map[string]any{"target": "https://b.example", "owner": "bob"})

	var all struct {
		Links []store.Link `json:"links"`
	}
	_ = json.NewDecoder(r.do(t, http.MethodGet, "/api/v1/links", token, nil).Body).Decode(&all)
	if len(all.Links) != 2 {
		t.Errorf("got %d links, want 2", len(all.Links))
	}

	var mine struct {
		Links []store.Link `json:"links"`
	}
	_ = json.NewDecoder(r.do(t, http.MethodGet, "/api/v1/links?owner=alice", token, nil).Body).Decode(&mine)
	if len(mine.Links) != 1 || mine.Links[0].Owner != "alice" {
		t.Errorf("owner filter returned %+v", mine.Links)
	}
}
