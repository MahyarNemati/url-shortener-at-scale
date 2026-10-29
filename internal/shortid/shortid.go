// Package shortid generates and encodes short URL slugs.
//
// The ID scheme is the first real design decision in a URL shortener, and the
// obvious options are all wrong in some way:
//
//   - **Random slugs** need a uniqueness check on every insert, which is a
//     database round trip before you can answer, and a retry loop under
//     contention.
//   - **A database sequence** is a single point of coordination: every write
//     waits on one row, and the slugs leak your total volume to anyone who can
//     count in base 62.
//   - **A hash of the URL** gives free deduplication but no way to have two
//     slugs for one URL, which breaks per-campaign analytics.
//
// This uses **Snowflake-style IDs**: 41 bits of millisecond timestamp, 10 bits
// of instance id, 12 bits of per-millisecond sequence. That gives every
// instance its own ID space with no coordination at all, 4096 IDs per
// millisecond per instance, and roughly 69 years of range.
//
// The cost, stated plainly: slugs are **monotonically increasing and therefore
// enumerable**. For a public shortener that is a real problem -- someone can
// walk your entire corpus. The fix is to obfuscate the ID before encoding
// (see Obfuscate), which keeps the coordination-free generation while making
// the next slug unguessable.
package shortid

import (
	"errors"
	"fmt"
	"math/bits"
	"strings"
	"sync"
	"time"
)

// Alphabet is base62. Chosen over base64 because a slug ends up in URLs,
// double-clicked to select, and read aloud over the phone: '+' and '/' need
// escaping and '-'/'_' break word selection in most terminals.
const Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

const base = uint64(len(Alphabet))

// Epoch is the custom epoch: 2024-01-01T00:00:00Z. Using a recent epoch
// rather than the Unix epoch buys back the ~54 years already elapsed, so the
// 41-bit timestamp lasts until ~2093 instead of ~2039.
var Epoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// Bit layout.
const (
	timestampBits = 41
	instanceBits  = 10
	sequenceBits  = 12

	maxInstance = (1 << instanceBits) - 1
	maxSequence = (1 << sequenceBits) - 1

	instanceShift  = sequenceBits
	timestampShift = sequenceBits + instanceBits
)

// MaxInstanceID is the largest valid instance id.
const MaxInstanceID = maxInstance

// Errors returned by this package.
var (
	ErrInstanceRange  = errors.New("shortid: instance id out of range")
	ErrClockBackwards = errors.New("shortid: clock moved backwards too far")
	ErrInvalidSlug    = errors.New("shortid: invalid slug")
)

// Generator produces IDs for one instance.
type Generator struct {
	instance uint64
	now      func() time.Time

	mu       sync.Mutex
	lastMs   int64
	sequence uint64
}

// NewGenerator returns a Generator for an instance id in [0, MaxInstanceID].
func NewGenerator(instance int) (*Generator, error) {
	if instance < 0 || instance > maxInstance {
		return nil, fmt.Errorf("%w: %d not in [0,%d]", ErrInstanceRange, instance, maxInstance)
	}
	return &Generator{instance: uint64(instance), now: time.Now}, nil
}

// WithClock overrides the time source, for tests.
func (g *Generator) WithClock(now func() time.Time) *Generator {
	g.now = now
	return g
}

// Next returns the next ID.
//
// It handles the two clock problems a Snowflake generator must:
//
//   - **Same millisecond, sequence exhausted**: spin until the next
//     millisecond rather than emitting a duplicate.
//   - **Clock moved backwards** (NTP step, VM migration): refuse rather than
//     emit an ID that collides with one already issued. A small backward step
//     is absorbed by waiting; a large one is an error, because silently
//     waiting minutes would look like a hang.
func (g *Generator) Next() (uint64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	ms := g.millis()

	if ms < g.lastMs {
		drift := time.Duration(g.lastMs-ms) * time.Millisecond
		// 5ms covers ordinary NTP slew; anything larger is a real problem and
		// should be visible rather than absorbed.
		if drift > 5*time.Millisecond {
			return 0, fmt.Errorf("%w: %s behind", ErrClockBackwards, drift)
		}
		ms = g.lastMs
	}

	if ms == g.lastMs {
		g.sequence++
		if g.sequence > maxSequence {
			// Exhausted this millisecond: wait for the next one. 4096 IDs per
			// millisecond per instance is 4M/s, so this is rare.
			ms = g.waitNextMilli(g.lastMs)
			g.sequence = 0
		}
	} else {
		g.sequence = 0
	}
	g.lastMs = ms

	elapsed := uint64(ms - Epoch.UnixMilli())
	if elapsed >= (1 << timestampBits) {
		return 0, errors.New("shortid: timestamp exceeds 41 bits; the epoch needs moving")
	}

	return elapsed<<timestampShift | g.instance<<instanceShift | g.sequence, nil
}

func (g *Generator) millis() int64 { return g.now().UnixMilli() }

func (g *Generator) waitNextMilli(after int64) int64 {
	for {
		ms := g.millis()
		if ms > after {
			return ms
		}
		// A Gosched rather than a sleep: the wait is sub-millisecond, and a
		// sleep would overshoot by the scheduler's granularity.
		time.Sleep(100 * time.Microsecond)
	}
}

// Decompose splits an ID back into its parts, which makes a slug
// self-describing for debugging: you can tell when and where it was minted.
func Decompose(id uint64) (ts time.Time, instance, sequence uint64) {
	elapsed := id >> timestampShift
	instance = (id >> instanceShift) & maxInstance
	sequence = id & maxSequence
	return Epoch.Add(time.Duration(elapsed) * time.Millisecond), instance, sequence
}

// Encode renders an ID in base62, most significant digit first.
func Encode(id uint64) string {
	if id == 0 {
		return Alphabet[:1]
	}
	// 11 base62 digits cover the full uint64 range.
	buf := make([]byte, 0, 11)
	for id > 0 {
		buf = append(buf, Alphabet[id%base])
		id /= base
	}
	// Reverse in place.
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}

// decodeTable maps a byte to its base62 value, or 0xff when invalid. Built
// once rather than scanning the alphabet per character.
var decodeTable = func() [256]byte {
	var t [256]byte
	for i := range t {
		t[i] = 0xff
	}
	for i := 0; i < len(Alphabet); i++ {
		t[Alphabet[i]] = byte(i)
	}
	return t
}()

// Decode parses a base62 slug.
func Decode(slug string) (uint64, error) {
	if slug == "" {
		return 0, fmt.Errorf("%w: empty", ErrInvalidSlug)
	}
	if len(slug) > 11 {
		return 0, fmt.Errorf("%w: %q is too long", ErrInvalidSlug, slug)
	}
	var id uint64
	for i := 0; i < len(slug); i++ {
		v := decodeTable[slug[i]]
		if v == 0xff {
			return 0, fmt.Errorf("%w: %q contains %q", ErrInvalidSlug, slug, slug[i])
		}
		// Reject overflow rather than wrapping, so a crafted slug cannot
		// alias a real one.
		hi, lo := bits.Mul64(id, base)
		if hi != 0 {
			return 0, fmt.Errorf("%w: %q overflows", ErrInvalidSlug, slug)
		}
		sum, carry := bits.Add64(lo, uint64(v), 0)
		if carry != 0 {
			return 0, fmt.Errorf("%w: %q overflows", ErrInvalidSlug, slug)
		}
		id = sum
	}
	return id, nil
}

// Obfuscator hides the sequential structure of an ID.
//
// Without this, slugs are enumerable: mint one, decode it, and you can walk
// every link in the system. With it, generation stays coordination-free while
// the next slug is unguessable without the key.
//
// This is a Feistel network over 64 bits, which is a *bijection*: every ID
// maps to exactly one obfuscated value and back, so there are still no
// collisions and no lookup table. It is deliberately **not** encryption --
// 4 rounds of a multiply-xor round function is not a cipher, and it does not
// need to be. The requirement is "not trivially enumerable", not "secret".
type Obfuscator struct {
	keys [4]uint64
}

// NewObfuscator derives round keys from a secret. An empty secret returns nil,
// which the callers treat as "no obfuscation", so the feature is opt-in.
func NewObfuscator(secret string) *Obfuscator {
	if secret == "" {
		return nil
	}
	o := &Obfuscator{}
	// splitmix64 over the secret bytes: cheap, and good enough to spread a
	// short passphrase across four round keys.
	var state uint64 = 0x9E3779B97F4A7C15
	for i := 0; i < len(secret); i++ {
		state += uint64(secret[i]) * 0x9E3779B97F4A7C15
		state = splitmix64(state)
	}
	for i := range o.keys {
		state = splitmix64(state)
		o.keys[i] = state
	}
	return o
}

func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	z := x
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// Obfuscate maps an ID to its scrambled form.
func (o *Obfuscator) Obfuscate(id uint64) uint64 {
	if o == nil {
		return id
	}
	left, right := uint32(id>>32), uint32(id)
	for i := 0; i < 4; i++ {
		left, right = right, left^round(right, o.keys[i])
	}
	return uint64(left)<<32 | uint64(right)
}

// Deobfuscate inverts Obfuscate.
func (o *Obfuscator) Deobfuscate(v uint64) uint64 {
	if o == nil {
		return v
	}
	left, right := uint32(v>>32), uint32(v)
	// Run the rounds backwards.
	for i := 3; i >= 0; i-- {
		left, right = right^round(left, o.keys[i]), left
	}
	return uint64(left)<<32 | uint64(right)
}

func round(v uint32, key uint64) uint32 {
	x := uint64(v) ^ key
	x = splitmix64(x)
	return uint32(x)
}

// Normalize trims and lower-cases nothing: slugs are case sensitive, because
// case doubles the keyspace per character and base62 depends on it. This
// exists only to strip whitespace a copy-paste may have added.
func Normalize(slug string) string { return strings.TrimSpace(slug) }
