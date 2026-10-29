// Package analytics records click events.
//
// The constraint that shapes this: **a click must never wait on analytics**.
// The redirect is the product; recording the click is a side effect. So
// events go into a bounded buffer and are flushed in batches by a background
// worker, and when the buffer is full events are **dropped**, with a counter.
//
// Dropping is the right trade-off and worth being explicit about: the
// alternative is blocking the redirect, which turns an analytics outage into a
// site outage. A visible drop counter is how you find out it happened.
package analytics

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event is one click.
type Event struct {
	At     time.Time `json:"at"`
	Slug   string    `json:"slug"`
	Target string    `json:"target"`
	// Referer host only, not the full URL: the path of a referring page is
	// frequently sensitive and is not needed to answer "where did traffic
	// come from".
	RefererHost string `json:"referer_host,omitempty"`
	UserAgent   string `json:"user_agent,omitempty"`
	// Country is from a CDN-supplied header when present.
	Country string `json:"country,omitempty"`
	// Device is a coarse classification, not a parsed UA string.
	Device string `json:"device,omitempty"`
	// IPPrefix is the truncated client address: /24 for IPv4, /48 for IPv6.
	// Storing a full IP would make the click log personal data with no
	// analytical benefit, since geography and uniqueness survive truncation.
	IPPrefix string `json:"ip_prefix,omitempty"`
	// Status is the HTTP status served, so 404s and 410s are countable too.
	Status int `json:"status"`
}

// Sink receives batches of events.
type Sink interface {
	// WriteBatch persists events. It may be slow; the recorder calls it from
	// a background goroutine.
	WriteBatch(ctx context.Context, events []Event) error
	Close() error
}

// Stats describes recorder health.
type Stats struct {
	Enqueued uint64
	Written  uint64
	Dropped  uint64
	Failed   uint64
	Batches  uint64
}

// Recorder buffers events and flushes them in batches.
type Recorder struct {
	sink    Sink
	events  chan Event
	log     *slog.Logger
	batch   int
	flush   time.Duration
	timeout time.Duration

	enqueued, written, dropped, failed, batches atomic.Uint64

	wg   sync.WaitGroup
	once sync.Once
	done chan struct{}
}

// Options configure a Recorder.
type Options struct {
	// Buffer is the queue depth. Sized for the burst you want to absorb: at
	// 10k clicks/second, a 100k buffer covers a 10-second sink outage.
	Buffer int
	// BatchSize is how many events to accumulate before writing. ClickHouse
	// strongly prefers large batches: thousands of single-row inserts create
	// thousands of parts and the merge load becomes the bottleneck.
	BatchSize int
	// FlushInterval bounds how long an event waits when traffic is low.
	FlushInterval time.Duration
	// WriteTimeout bounds one batch write.
	WriteTimeout time.Duration
	Logger       *slog.Logger
}

// NewRecorder starts a Recorder.
func NewRecorder(sink Sink, opts Options) *Recorder {
	if opts.Buffer <= 0 {
		opts.Buffer = 100000
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 5000
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = 2 * time.Second
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = 30 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	r := &Recorder{
		sink: sink, events: make(chan Event, opts.Buffer), log: opts.Logger,
		batch: opts.BatchSize, flush: opts.FlushInterval, timeout: opts.WriteTimeout,
		done: make(chan struct{}),
	}
	r.wg.Add(1)
	go r.run()
	return r
}

// Record enqueues an event. It never blocks: a full buffer drops the event
// and increments a counter, because blocking here would make the redirect wait
// on the analytics backend.
func (r *Recorder) Record(e Event) {
	r.enqueued.Add(1)
	select {
	case r.events <- e:
	default:
		r.dropped.Add(1)
	}
}

func (r *Recorder) run() {
	defer r.wg.Done()

	buf := make([]Event, 0, r.batch)
	ticker := time.NewTicker(r.flush)
	defer ticker.Stop()

	write := func() {
		if len(buf) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
		err := r.sink.WriteBatch(ctx, buf)
		cancel()

		r.batches.Add(1)
		if err != nil {
			r.failed.Add(uint64(len(buf)))
			// Events are not retried. Retrying would need a durable queue,
			// and silently growing an in-memory retry list is how an
			// analytics hiccup becomes an OOM kill.
			r.log.Error("analytics batch failed; events dropped",
				"events", len(buf), "err", err)
		} else {
			r.written.Add(uint64(len(buf)))
		}
		buf = buf[:0]
	}

	for {
		select {
		case <-r.done:
			// Drain whatever is queued before exiting.
			for {
				select {
				case e := <-r.events:
					buf = append(buf, e)
					if len(buf) >= r.batch {
						write()
					}
				default:
					write()
					return
				}
			}
		case e := <-r.events:
			buf = append(buf, e)
			if len(buf) >= r.batch {
				write()
			}
		case <-ticker.C:
			write()
		}
	}
}

// Close flushes and stops the recorder.
func (r *Recorder) Close() error {
	r.once.Do(func() { close(r.done) })
	r.wg.Wait()
	return r.sink.Close()
}

// Stats returns the counters.
func (r *Recorder) Stats() Stats {
	return Stats{
		Enqueued: r.enqueued.Load(),
		Written:  r.written.Load(),
		Dropped:  r.dropped.Load(),
		Failed:   r.failed.Load(),
		Batches:  r.batches.Load(),
	}
}

// FromRequest builds an Event from an HTTP request, keeping only what is
// needed and truncating what is sensitive.
func FromRequest(r *http.Request, slug, target string, status int, now time.Time, trustProxy bool) Event {
	e := Event{
		At: now, Slug: slug, Target: target, Status: status,
		UserAgent: truncate(r.UserAgent(), 400),
		Device:    classifyDevice(r.UserAgent()),
		Country:   firstHeader(r, "CF-IPCountry", "X-Country-Code", "Fastly-Geo-Country"),
		IPPrefix:  ipPrefix(clientIP(r, trustProxy)),
	}
	if ref := r.Referer(); ref != "" {
		if u, err := url.Parse(ref); err == nil {
			e.RefererHost = u.Host
		}
	}
	return e
}

// clientIP returns the peer address, consulting X-Forwarded-For only when a
// trusted proxy is in front. The header is client-controlled, so trusting it
// unconditionally lets a caller forge their own geography.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first, _, found := strings.Cut(xff, ","); found {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ipPrefix truncates an address to /24 (IPv4) or /48 (IPv6).
//
// This is the whole privacy story: geography and rough uniqueness survive
// truncation, while the result is no longer an identifier for a person. It
// also means the click log does not become a GDPR problem that has to be
// deleted on request.
func ipPrefix(addr string) string {
	ip := net.ParseIP(addr)
	if ip == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return net.IP{v4[0], v4[1], v4[2], 0}.String() + "/24"
	}
	masked := ip.Mask(net.CIDRMask(48, 128))
	return masked.String() + "/48"
}

// classifyDevice is a coarse bucket, not a UA parser. Full UA parsing needs a
// regularly-updated database and answers a question nobody asks; "mobile,
// tablet, desktop or bot" is what a traffic report actually uses.
func classifyDevice(ua string) string {
	l := strings.ToLower(ua)
	switch {
	case l == "":
		return "unknown"
	case strings.Contains(l, "bot"), strings.Contains(l, "crawler"),
		strings.Contains(l, "spider"), strings.Contains(l, "curl"),
		strings.Contains(l, "wget"), strings.Contains(l, "python-requests"),
		strings.Contains(l, "headless"):
		return "bot"
	case strings.Contains(l, "ipad"), strings.Contains(l, "tablet"):
		return "tablet"
	case strings.Contains(l, "mobile"), strings.Contains(l, "android"),
		strings.Contains(l, "iphone"):
		return "mobile"
	default:
		return "desktop"
	}
}

func firstHeader(r *http.Request, names ...string) string {
	for _, n := range names {
		if v := r.Header.Get(n); v != "" {
			return truncate(v, 8)
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Discard is a Sink that counts and throws away, used when analytics are
// disabled and in tests.
type Discard struct{ count atomic.Uint64 }

// WriteBatch counts the events.
func (d *Discard) WriteBatch(_ context.Context, events []Event) error {
	d.count.Add(uint64(len(events)))
	return nil
}

// Close is a no-op.
func (d *Discard) Close() error { return nil }

// Count returns how many events were discarded.
func (d *Discard) Count() uint64 { return d.count.Load() }

// ErrNoSink is returned when a sink is required but absent.
var ErrNoSink = errors.New("analytics: no sink configured")
