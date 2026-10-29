package shortid_test

import (
	"sync"
	"testing"
	"time"

	"github.com/MahyarNemati/url-shortener-at-scale/internal/shortid"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	for _, id := range []uint64{0, 1, 61, 62, 63, 3843, 3844, 1 << 32, 1 << 63, ^uint64(0)} {
		slug := shortid.Encode(id)
		got, err := shortid.Decode(slug)
		if err != nil {
			t.Fatalf("Decode(%q): %v", slug, err)
		}
		if got != id {
			t.Errorf("round trip %d -> %q -> %d", id, slug, got)
		}
	}
}

func TestEncodeIsCompact(t *testing.T) {
	// A Snowflake ID today is ~59 bits, which is 10 base62 characters. Longer
	// than that and the "short" in short URL stops being true.
	g, _ := shortid.NewGenerator(0)
	id, err := g.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	slug := shortid.Encode(id)
	if len(slug) > 11 {
		t.Errorf("slug %q is %d characters, expected at most 11", slug, len(slug))
	}
}

func TestDecodeRejectsInvalidInput(t *testing.T) {
	for _, slug := range []string{"", "has space", "has-dash", "a/b", "+", "ZZZZZZZZZZZZZZ"} {
		if _, err := shortid.Decode(slug); err == nil {
			t.Errorf("Decode(%q) should have failed", slug)
		}
	}
}

func TestDecodeRejectsOverflowRatherThanWrapping(t *testing.T) {
	// A crafted slug must not alias a real id by wrapping around uint64.
	if _, err := shortid.Decode("zzzzzzzzzzz"); err == nil {
		t.Error("an overflowing slug should be rejected")
	}
}

func TestGeneratorRejectsBadInstanceID(t *testing.T) {
	for _, id := range []int{-1, shortid.MaxInstanceID + 1, 5000} {
		if _, err := shortid.NewGenerator(id); err == nil {
			t.Errorf("instance %d should be rejected", id)
		}
	}
}

func TestIDsAreUniqueAndIncreasing(t *testing.T) {
	g, _ := shortid.NewGenerator(1)
	seen := make(map[uint64]bool, 50000)
	var last uint64
	for i := 0; i < 50000; i++ {
		id, err := g.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate id %d at iteration %d", id, i)
		}
		seen[id] = true
		if id <= last && last != 0 {
			t.Fatalf("id %d is not greater than previous %d", id, last)
		}
		last = id
	}
}

func TestIDsAreUniqueUnderConcurrency(t *testing.T) {
	g, _ := shortid.NewGenerator(2)
	const workers, perWorker = 8, 5000

	var mu sync.Mutex
	seen := make(map[uint64]bool, workers*perWorker)
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]uint64, 0, perWorker)
			for i := 0; i < perWorker; i++ {
				id, err := g.Next()
				if err != nil {
					t.Errorf("Next: %v", err)
					return
				}
				local = append(local, id)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range local {
				if seen[id] {
					t.Errorf("duplicate id %d across goroutines", id)
					return
				}
				seen[id] = true
			}
		}()
	}
	wg.Wait()

	if len(seen) != workers*perWorker {
		t.Errorf("got %d unique ids, want %d", len(seen), workers*perWorker)
	}
}

func TestDifferentInstancesNeverCollide(t *testing.T) {
	// The whole point of the instance field: two processes generate IDs with
	// no coordination and cannot collide.
	a, _ := shortid.NewGenerator(0)
	b, _ := shortid.NewGenerator(1)

	seen := map[uint64]bool{}
	for i := 0; i < 20000; i++ {
		for _, g := range []*shortid.Generator{a, b} {
			id, err := g.Next()
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if seen[id] {
				t.Fatalf("collision between instances at %d", id)
			}
			seen[id] = true
		}
	}
}

func TestSequenceExhaustionWaitsForTheNextMillisecond(t *testing.T) {
	// Pin the clock so the 4096-per-millisecond sequence is exhausted, then
	// let it advance. The generator must wait rather than emit a duplicate.
	var mu sync.Mutex
	now := time.Now()
	g, _ := shortid.NewGenerator(0)
	g.WithClock(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	})

	// Advance the clock from another goroutine, simulating time passing while
	// the generator spins.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				mu.Lock()
				now = now.Add(time.Millisecond)
				mu.Unlock()
				time.Sleep(200 * time.Microsecond)
			}
		}
	}()
	defer close(stop)

	seen := map[uint64]bool{}
	for i := 0; i < 20000; i++ {
		id, err := g.Next()
		if err != nil {
			t.Fatalf("Next at %d: %v", i, err)
		}
		if seen[id] {
			t.Fatalf("duplicate id after sequence exhaustion at %d", i)
		}
		seen[id] = true
	}
}

func TestClockMovingBackwardsIsRefused(t *testing.T) {
	// An NTP step or VM migration must not silently produce a colliding id.
	now := time.Now()
	g, _ := shortid.NewGenerator(0)
	g.WithClock(func() time.Time { return now })

	if _, err := g.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}

	// A small step is absorbed.
	now = now.Add(-2 * time.Millisecond)
	if _, err := g.Next(); err != nil {
		t.Errorf("a 2ms backward step should be absorbed, got %v", err)
	}

	// A large one is an error, because silently waiting would look like a hang.
	now = now.Add(-10 * time.Second)
	if _, err := g.Next(); err == nil {
		t.Error("a 10s backward clock step should be refused")
	}
}

func TestDecomposeRecoversTheParts(t *testing.T) {
	g, _ := shortid.NewGenerator(7)
	before := time.Now()
	id, err := g.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	after := time.Now()

	ts, instance, _ := shortid.Decompose(id)
	if instance != 7 {
		t.Errorf("instance = %d, want 7", instance)
	}
	// A slug being self-describing is genuinely useful when debugging "when
	// was this link made and by which instance".
	if ts.Before(before.Add(-time.Second)) || ts.After(after.Add(time.Second)) {
		t.Errorf("timestamp %s is not within the generation window", ts)
	}
}

func TestNormalizeOnlyTrimsWhitespace(t *testing.T) {
	// Slugs are case sensitive: case doubles the keyspace per character and
	// base62 depends on it, so normalising case would collapse distinct links.
	if got := shortid.Normalize("  AbC123 \n"); got != "AbC123" {
		t.Errorf("Normalize = %q, want %q", got, "AbC123")
	}
}

func BenchmarkNext(b *testing.B) {
	g, _ := shortid.NewGenerator(0)
	for i := 0; i < b.N; i++ {
		if _, err := g.Next(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncode(b *testing.B) {
	const id = 373989100576833536
	for i := 0; i < b.N; i++ {
		shortid.Encode(id)
	}
}

func BenchmarkObfuscateEncode(b *testing.B) {
	o := shortid.NewObfuscator("bench")
	const id = 373989100576833536
	for i := 0; i < b.N; i++ {
		shortid.Encode(o.Obfuscate(id))
	}
}
