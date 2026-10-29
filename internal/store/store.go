// Package store persists links.
package store

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Errors returned by a Store.
var (
	ErrNotFound = errors.New("store: link not found")
	ErrConflict = errors.New("store: slug already taken")
	// ErrGone marks a link that existed and was deleted. Distinguishing it
	// from ErrNotFound lets the API answer 410 rather than 404, which tells a
	// caller the difference between "never existed" and "retired".
	ErrGone = errors.New("store: link was deleted")
)

// Link is a shortened URL.
type Link struct {
	// ID is the numeric id the slug encodes.
	ID uint64 `json:"id"`
	// Slug is the base62 (possibly obfuscated) short code.
	Slug string `json:"slug"`
	// Target is the destination URL.
	Target string `json:"target"`
	// CreatedAt is when the link was minted.
	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt, when non-zero, is when the link stops resolving.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// Owner identifies who created it, from the API token.
	Owner string `json:"owner,omitempty"`
	// Deleted marks a retired link. Rows are kept rather than removed so a
	// retired slug is never reissued and analytics keep their referent.
	Deleted bool `json:"deleted,omitempty"`
	// Tags are free-form labels, e.g. a campaign name.
	Tags []string `json:"tags,omitempty"`
}

// Expired reports whether the link has passed its expiry.
func (l *Link) Expired(now time.Time) bool {
	return !l.ExpiresAt.IsZero() && now.After(l.ExpiresAt)
}

// Resolvable reports whether the link should redirect.
func (l *Link) Resolvable(now time.Time) bool {
	return l != nil && !l.Deleted && !l.Expired(now)
}

// Store persists links.
type Store interface {
	// Create stores a new link. ErrConflict when the slug is taken.
	Create(ctx context.Context, l *Link) error
	// Get returns a link by slug, including deleted ones, so the caller can
	// distinguish 404 from 410.
	Get(ctx context.Context, slug string) (*Link, error)
	// Delete marks a link deleted.
	Delete(ctx context.Context, slug string) error
	// List returns links for an owner, newest first.
	List(ctx context.Context, owner string, limit int) ([]*Link, error)
	Ping(ctx context.Context) error
	Close() error
}

// Memory is an in-process Store, for development and tests.
type Memory struct {
	mu     sync.RWMutex
	bySlug map[string]*Link
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{bySlug: map[string]*Link{}}
}

func (m *Memory) Create(_ context.Context, l *Link) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.bySlug[l.Slug]; exists {
		return ErrConflict
	}
	copied := *l
	copied.Tags = append([]string(nil), l.Tags...)
	m.bySlug[l.Slug] = &copied
	return nil
}

func (m *Memory) Get(_ context.Context, slug string) (*Link, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	l, ok := m.bySlug[slug]
	if !ok {
		return nil, ErrNotFound
	}
	copied := *l
	copied.Tags = append([]string(nil), l.Tags...)
	return &copied, nil
}

func (m *Memory) Delete(_ context.Context, slug string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.bySlug[slug]
	if !ok {
		return ErrNotFound
	}
	l.Deleted = true
	return nil
}

func (m *Memory) List(_ context.Context, owner string, limit int) ([]*Link, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Link
	for _, l := range m.bySlug {
		if owner != "" && l.Owner != owner {
			continue
		}
		copied := *l
		out = append(out, &copied)
	}
	// Newest first.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.After(out[j-1].CreatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) Ping(context.Context) error { return nil }
func (m *Memory) Close() error               { return nil }

// Len reports how many links are stored, for tests.
func (m *Memory) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.bySlug)
}
