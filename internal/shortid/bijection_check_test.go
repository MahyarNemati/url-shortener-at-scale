package shortid

import (
	"math/rand"
	"testing"
)

// The bijection property is load-bearing: if Obfuscate ever maps two IDs to
// the same value, two different links share a slug.
func TestObfuscatorIsABijection(t *testing.T) {
	o := NewObfuscator("test-secret")

	for i := 0; i < 200000; i++ {
		id := rand.Uint64()
		if got := o.Deobfuscate(o.Obfuscate(id)); got != id {
			t.Fatalf("round trip failed: %d -> %d -> %d", id, o.Obfuscate(id), got)
		}
	}
	// Small sequential IDs are the real use case, and the case a careless
	// round function handles worst.
	for i := uint64(0); i < 100000; i++ {
		if got := o.Deobfuscate(o.Obfuscate(i)); got != i {
			t.Fatalf("round trip failed for sequential %d -> %d", i, got)
		}
	}
}

func TestObfuscatorHasNoCollisionsOverASequentialRange(t *testing.T) {
	o := NewObfuscator("test-secret")
	seen := make(map[uint64]uint64, 200000)
	for i := uint64(0); i < 200000; i++ {
		v := o.Obfuscate(i)
		if prev, dup := seen[v]; dup {
			t.Fatalf("collision: %d and %d both map to %d", prev, i, v)
		}
		seen[v] = i
	}
}

func TestObfuscationBreaksSequentialStructure(t *testing.T) {
	o := NewObfuscator("test-secret")
	// Consecutive IDs must not produce consecutive slugs, or the whole point
	// is lost.
	a, b := o.Obfuscate(1000), o.Obfuscate(1001)
	diff := a - b
	if diff == 1 || b-a == 1 {
		t.Errorf("consecutive IDs produced adjacent values: %d and %d", a, b)
	}
	if Encode(a)[:1] == Encode(b)[:1] && len(Encode(a)) == len(Encode(b)) {
		t.Logf("note: slugs %q and %q share a prefix; acceptable but worth seeing", Encode(a), Encode(b))
	}
}

func TestNilObfuscatorIsIdentity(t *testing.T) {
	var o *Obfuscator
	for _, id := range []uint64{0, 1, 1 << 40, ^uint64(0)} {
		if got := o.Obfuscate(id); got != id {
			t.Errorf("nil obfuscator changed %d to %d", id, got)
		}
		if got := o.Deobfuscate(id); got != id {
			t.Errorf("nil deobfuscator changed %d to %d", id, got)
		}
	}
}

func TestDifferentSecretsProduceDifferentMappings(t *testing.T) {
	a, b := NewObfuscator("secret-a"), NewObfuscator("secret-b")
	same := 0
	for i := uint64(0); i < 1000; i++ {
		if a.Obfuscate(i) == b.Obfuscate(i) {
			same++
		}
	}
	if same > 2 {
		t.Errorf("%d of 1000 values mapped identically under different secrets", same)
	}
}
