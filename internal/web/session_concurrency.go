package web

import (
	"context"
	"sync"
)

// sessionConcurrency serializes requests that target the same logical session
// while allowing requests for different sessions to run concurrently.
type sessionConcurrency struct {
	mu      sync.Mutex
	entries map[string]*sessionConcurrencyEntry
}

type sessionConcurrencyEntry struct {
	gate chan struct{}
	refs int
}

func newSessionConcurrency() *sessionConcurrency {
	return &sessionConcurrency{entries: make(map[string]*sessionConcurrencyEntry)}
}

// Acquire waits until no other request is using key. The returned release
// function is idempotent and must be called when the request has completed.
func (c *sessionConcurrency) Acquire(ctx context.Context, key string) (func(), error) {
	if c == nil || key == "" {
		return func() {}, nil
	}

	c.mu.Lock()
	entry := c.entries[key]
	if entry == nil {
		entry = &sessionConcurrencyEntry{gate: make(chan struct{}, 1)}
		entry.gate <- struct{}{}
		c.entries[key] = entry
	}
	entry.refs++
	c.mu.Unlock()

	select {
	case <-ctx.Done():
		c.dropReference(key, entry)
		return nil, ctx.Err()
	case <-entry.gate:
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			entry.gate <- struct{}{}
			c.dropReference(key, entry)
		})
	}, nil
}

func (c *sessionConcurrency) dropReference(key string, entry *sessionConcurrencyEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.refs--
	if entry.refs == 0 && c.entries[key] == entry {
		delete(c.entries, key)
	}
}
