// Package cache fronts the store on the redirect hot path.
//
// The redirect is the only latency that matters in a URL shortener: creating a
// link happens once, resolving it happens every click. So the hot path is
// designed around three things:
//
//  1. **Cache negative lookups too.** Without that, a crawler walking random
//     slugs sends every miss straight to the database — which is exactly the
//     load pattern a shortener attracts, and the reason "we cache hot links"
//     is not enough.
//  2. **Single-flight the misses.** A viral link expiring from cache would
//     otherwise send every concurrent request to the database at once, a
//     stampede precisely when traffic is highest.
//  3. **Serve stale on a backend error.** A slightly out-of-date redirect is
//     better than a 500, and a cached link is almost always still correct.
package cache

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/MahyarNemati/url-shortener-at-scale/internal/store"
)

// Stats counts cache outcomes, for the /metrics endpoint and for proving the
// cache is actually doing something.
type Stats struct {
	Hits         uint64
	Misses       uint64
	NegativeHits uint64
	Stale        uint64
	Coalesced    uint64
	Evictions    uint64
}

// entry is a cached lookup. A nil link with no error is a cached negative.
type entry struct {
	link    *store.Link
	expires time.Time
	// negative marks "this slug does not exist", cached separately so a miss
	// storm cannot reach the database.
	negative bool
}

// Cache is an in-process LRU with single-flight and negative caching.
//
// In-process rather than Redis for the first layer: a local map lookup is
// ~50ns against ~200µs for a Redis round trip, and the working set of a
// shortener is small enough to hold. Redis is the *second* layer, shared
// between instances; this is the first.
type Cache struct {
	backend  store.Store
	ttl      time.Duration
	negTTL   time.Duration
	capacity int
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]*entry
	// order is an approximate LRU: a ring of keys, evicting the oldest
	// inserted. True LRU needs a linked list and more bookkeeping than the
	// hit-rate difference justifies here.
	order []string

	flight map[string]*call

	statsMu sync.Mutex
	stats   Stats
}

type call struct {
	wg   sync.WaitGroup
	link *store.Link
	err  error
}

// Options configure a Cache.
type Options struct {
	// TTL is how long a found link is cached.
	TTL time.Duration
	// NegativeTTL is how long a "not found" is cached. Deliberately shorter
	// than TTL: a slug that does not exist yet may be created imminently, and
	// a long negative TTL would make a new link appear broken.
	NegativeTTL time.Duration
	Capacity    int
	Now         func() time.Time
}

// New returns a Cache in front of backend.
func New(backend store.Store, opts Options) *Cache {
	if opts.TTL <= 0 {
		opts.TTL = 5 * time.Minute
	}
	if opts.NegativeTTL <= 0 {
		opts.NegativeTTL = 10 * time.Second
	}
	if opts.Capacity <= 0 {
		opts.Capacity = 100000
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Cache{
		backend: backend, ttl: opts.TTL, negTTL: opts.NegativeTTL,
		capacity: opts.Capacity, now: opts.Now,
		entries: make(map[string]*entry, opts.Capacity/4),
		flight:  map[string]*call{},
	}
}

// Get resolves a slug, consulting the cache first.
func (c *Cache) Get(ctx context.Context, slug string) (*store.Link, error) {
	now := c.now()

	c.mu.Lock()
	e, ok := c.entries[slug]
	if ok && now.Before(e.expires) {
		c.mu.Unlock()
		if e.negative {
			c.count(func(s *Stats) { s.NegativeHits++ })
			return nil, store.ErrNotFound
		}
		c.count(func(s *Stats) { s.Hits++ })
		return e.link, nil
	}
	// Keep any expired entry around: it is the stale value to fall back on if
	// the backend fails.
	stale := e
	c.mu.Unlock()

	c.count(func(s *Stats) { s.Misses++ })

	link, err := c.load(ctx, slug)
	if err != nil {
		// Serve stale rather than an error. A slightly out-of-date redirect
		// beats a 500, and the link is almost certainly still valid.
		if stale != nil && !stale.negative && stale.link != nil && !errors.Is(err, store.ErrNotFound) {
			c.count(func(s *Stats) { s.Stale++ })
			return stale.link, nil
		}
		if errors.Is(err, store.ErrNotFound) {
			c.put(slug, &entry{negative: true, expires: now.Add(c.negTTL)})
		}
		return nil, err
	}

	c.put(slug, &entry{link: link, expires: now.Add(c.ttl)})
	return link, nil
}

// load fetches from the backend, collapsing concurrent misses for one slug
// into a single query.
func (c *Cache) load(ctx context.Context, slug string) (*store.Link, error) {
	c.mu.Lock()
	if inflight, ok := c.flight[slug]; ok {
		c.mu.Unlock()
		c.count(func(s *Stats) { s.Coalesced++ })
		inflight.wg.Wait()
		return inflight.link, inflight.err
	}
	cl := &call{}
	cl.wg.Add(1)
	c.flight[slug] = cl
	c.mu.Unlock()

	cl.link, cl.err = c.backend.Get(ctx, slug)
	cl.wg.Done()

	c.mu.Lock()
	delete(c.flight, slug)
	c.mu.Unlock()

	return cl.link, cl.err
}

func (c *Cache) put(slug string, e *entry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.entries[slug]; !exists {
		if len(c.order) >= c.capacity {
			// Evict the oldest inserted.
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.entries, oldest)
			c.statsMu.Lock()
			c.stats.Evictions++
			c.statsMu.Unlock()
		}
		c.order = append(c.order, slug)
	}
	c.entries[slug] = e
}

// Invalidate drops a slug, so a create or delete is visible immediately
// rather than after the TTL.
func (c *Cache) Invalidate(slug string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, slug)
}

// Stats returns a snapshot of the counters.
func (c *Cache) Stats() Stats {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	return c.stats
}

// HitRate returns hits as a fraction of all lookups, or -1 with no data.
func (s Stats) HitRate() float64 {
	total := s.Hits + s.NegativeHits + s.Misses
	if total == 0 {
		return -1
	}
	return float64(s.Hits+s.NegativeHits) / float64(total)
}

// Len reports the number of cached entries.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *Cache) count(fn func(*Stats)) {
	c.statsMu.Lock()
	fn(&c.stats)
	c.statsMu.Unlock()
}
