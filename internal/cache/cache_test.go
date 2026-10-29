package cache_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MahyarNemati/url-shortener-at-scale/internal/cache"
	"github.com/MahyarNemati/url-shortener-at-scale/internal/store"
)

// counting wraps a store and counts Get calls, so a test can prove the cache
// actually prevented a backend query.
type counting struct {
	inner    store.Store
	gets     atomic.Int64
	delay    time.Duration
	failWith error
	mu       sync.Mutex
}

func (c *counting) Get(ctx context.Context, slug string) (*store.Link, error) {
	c.gets.Add(1)
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	c.mu.Lock()
	err := c.failWith
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return c.inner.Get(ctx, slug)
}

func (c *counting) setFailure(err error) {
	c.mu.Lock()
	c.failWith = err
	c.mu.Unlock()
}

func (c *counting) Create(ctx context.Context, l *store.Link) error { return c.inner.Create(ctx, l) }
func (c *counting) Delete(ctx context.Context, slug string) error   { return c.inner.Delete(ctx, slug) }
func (c *counting) List(ctx context.Context, owner string, n int) ([]*store.Link, error) {
	return c.inner.List(ctx, owner, n)
}
func (c *counting) Ping(ctx context.Context) error { return c.inner.Ping(ctx) }
func (c *counting) Close() error                   { return c.inner.Close() }

func backend(t *testing.T, links ...*store.Link) *counting {
	t.Helper()
	m := store.NewMemory()
	for _, l := range links {
		if err := m.Create(context.Background(), l); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return &counting{inner: m}
}

func link(slug, target string) *store.Link {
	return &store.Link{Slug: slug, Target: target, CreatedAt: time.Now()}
}

func TestHitAvoidsTheBackend(t *testing.T) {
	b := backend(t, link("abc", "https://example.com"))
	c := cache.New(b, cache.Options{TTL: time.Minute})

	for i := 0; i < 100; i++ {
		got, err := c.Get(context.Background(), "abc")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Target != "https://example.com" {
			t.Fatalf("Target = %q", got.Target)
		}
	}
	// 100 lookups, one query.
	if n := b.gets.Load(); n != 1 {
		t.Errorf("backend queried %d times, want 1", n)
	}
	if rate := c.Stats().HitRate(); rate < 0.98 {
		t.Errorf("hit rate = %.3f, want > 0.98", rate)
	}
}

func TestNegativeLookupsAreCached(t *testing.T) {
	// The load pattern a shortener actually attracts: a crawler walking
	// random slugs. Without negative caching every miss hits the database.
	b := backend(t)
	c := cache.New(b, cache.Options{TTL: time.Minute, NegativeTTL: time.Minute})

	for i := 0; i < 100; i++ {
		_, err := c.Get(context.Background(), "nope")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	}
	if n := b.gets.Load(); n != 1 {
		t.Errorf("backend queried %d times for a missing slug, want 1", n)
	}
	if c.Stats().NegativeHits != 99 {
		t.Errorf("NegativeHits = %d, want 99", c.Stats().NegativeHits)
	}
}

func TestNegativeTTLIsShorterSoNewLinksAppearQuickly(t *testing.T) {
	now := time.Now()
	b := backend(t)
	c := cache.New(b, cache.Options{
		TTL: time.Hour, NegativeTTL: 10 * time.Second,
		Now: func() time.Time { return now },
	})

	_, _ = c.Get(context.Background(), "soon")

	// Create it after the negative was cached.
	_ = b.Create(context.Background(), link("soon", "https://example.com"))

	// Still cached as missing...
	if _, err := c.Get(context.Background(), "soon"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected the cached negative, got %v", err)
	}
	// ...but only briefly.
	now = now.Add(11 * time.Second)
	got, err := c.Get(context.Background(), "soon")
	if err != nil {
		t.Fatalf("after the negative TTL: %v", err)
	}
	if got.Target != "https://example.com" {
		t.Errorf("Target = %q", got.Target)
	}
}

func TestConcurrentMissesAreCoalescedIntoOneQuery(t *testing.T) {
	// A viral link expiring from cache would otherwise send every concurrent
	// request to the database at once -- a stampede exactly when traffic is
	// highest.
	b := backend(t, link("viral", "https://example.com"))
	b.delay = 50 * time.Millisecond
	c := cache.New(b, cache.Options{TTL: time.Minute})

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Get(context.Background(), "viral"); err != nil {
				t.Errorf("Get: %v", err)
			}
		}()
	}
	wg.Wait()

	if n := b.gets.Load(); n != 1 {
		t.Errorf("backend queried %d times for 200 concurrent misses, want 1", n)
	}
	if c.Stats().Coalesced == 0 {
		t.Error("expected the coalesced counter to be non-zero")
	}
}

func TestStaleIsServedWhenTheBackendFails(t *testing.T) {
	now := time.Now()
	b := backend(t, link("abc", "https://example.com"))
	c := cache.New(b, cache.Options{
		TTL: 10 * time.Second,
		Now: func() time.Time { return now },
	})

	if _, err := c.Get(context.Background(), "abc"); err != nil {
		t.Fatalf("warm: %v", err)
	}

	// Expire the entry and break the backend.
	now = now.Add(time.Minute)
	b.setFailure(errors.New("database is down"))

	got, err := c.Get(context.Background(), "abc")
	if err != nil {
		t.Fatalf("expected a stale hit, got %v", err)
	}
	if got.Target != "https://example.com" {
		t.Errorf("Target = %q", got.Target)
	}
	if c.Stats().Stale == 0 {
		t.Error("expected the stale counter to be non-zero")
	}
}

func TestNotFoundIsNotMaskedByStaleLogic(t *testing.T) {
	// A genuine ErrNotFound must still be reported as missing, not papered
	// over with a stale value.
	b := backend(t)
	c := cache.New(b, cache.Options{TTL: time.Minute})
	if _, err := c.Get(context.Background(), "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestInvalidateMakesAChangeVisibleImmediately(t *testing.T) {
	b := backend(t, link("abc", "https://old.example"))
	c := cache.New(b, cache.Options{TTL: time.Hour})

	if _, err := c.Get(context.Background(), "abc"); err != nil {
		t.Fatalf("warm: %v", err)
	}

	// Delete it. Without invalidation the link keeps redirecting for an hour,
	// which for a link removed because it points somewhere harmful is not
	// acceptable.
	_ = b.Delete(context.Background(), "abc")
	c.Invalidate("abc")

	got, err := c.Get(context.Background(), "abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Deleted {
		t.Error("expected the deleted flag after invalidation")
	}
}

func TestEvictionBoundsMemory(t *testing.T) {
	b := store.NewMemory()
	for i := 0; i < 200; i++ {
		_ = b.Create(context.Background(), link(slugOf(i), "https://example.com"))
	}
	c := cache.New(b, cache.Options{TTL: time.Hour, Capacity: 50})

	for i := 0; i < 200; i++ {
		if _, err := c.Get(context.Background(), slugOf(i)); err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
	}
	if n := c.Len(); n > 50 {
		t.Errorf("cache holds %d entries, want at most 50", n)
	}
	if c.Stats().Evictions == 0 {
		t.Error("expected evictions")
	}
}

func slugOf(i int) string {
	const digits = "abcdefghijklmnopqrstuvwxyz"
	return string([]byte{digits[i/26%26], digits[i%26], 'x'})
}

func TestConcurrentGetsAreRaceFree(t *testing.T) {
	b := backend(t, link("abc", "https://example.com"))
	c := cache.New(b, cache.Options{TTL: 10 * time.Millisecond, Capacity: 8})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, _ = c.Get(context.Background(), "abc")
				_, _ = c.Get(context.Background(), slugOf(j))
				if j%20 == 0 {
					c.Invalidate("abc")
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestHitRateIsMinusOneWithNoData(t *testing.T) {
	var s cache.Stats
	if got := s.HitRate(); got != -1 {
		t.Errorf("HitRate = %v, want -1 for no data", got)
	}
}
