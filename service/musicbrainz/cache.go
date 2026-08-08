package musicbrainz

import (
	"sync"
	"time"
)

// ttlCache is a size-capped map whose entries expire. Both the recording search
// and the release group pressing lookups are read-mostly and want the same
// eviction policy, so they share this rather than each hand-rolling it.
//
// The TTL is given per put rather than fixed on the cache: how long a result
// stays good is a property of the result, not of the cache holding it, and
// keeping it at the call site is what makes the short TTL on empty search
// results visible where the decision is made.
type ttlCache[V any] struct {
	mu      sync.RWMutex
	entries map[string]ttlEntry[V]
	maxSize int
}

type ttlEntry[V any] struct {
	value     V
	expiresAt time.Time
}

func newTTLCache[V any](maxSize int) *ttlCache[V] {
	return &ttlCache[V]{
		entries: make(map[string]ttlEntry[V]),
		maxSize: maxSize,
	}
}

// get returns the live value for a key. An expired entry is dropped on the way
// out, so a key that is read but never written again does not linger.
func (c *ttlCache[V]) get(key string) (V, bool) {
	var zero V

	c.mu.RLock()
	entry, found := c.entries[key]
	c.mu.RUnlock()

	if !found {
		return zero, false
	}
	if time.Now().UTC().Before(entry.expiresAt) {
		return entry.value, true
	}

	c.mu.Lock()
	// Re-check under the write lock: another goroutine may have refreshed the
	// entry since the read above.
	if entry, found := c.entries[key]; found && !time.Now().UTC().Before(entry.expiresAt) {
		delete(c.entries, key)
	}
	c.mu.Unlock()
	return zero, false
}

// put stores a value for ttl, sweeping expired entries and capping the size.
func (c *ttlCache[V]) put(key string, value V, ttl time.Duration) {
	now := time.Now().UTC()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Opportunistically sweep expired entries so the cache doesn't grow unbounded.
	for key, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, key)
		}
	}

	// Still full after sweeping: evict whatever is closest to expiring anyway.
	if len(c.entries) >= c.maxSize {
		var oldestKey string
		var oldestExpiry time.Time
		for key, entry := range c.entries {
			if oldestKey == "" || entry.expiresAt.Before(oldestExpiry) {
				oldestKey, oldestExpiry = key, entry.expiresAt
			}
		}
		delete(c.entries, oldestKey)
	}

	c.entries[key] = ttlEntry[V]{value: value, expiresAt: now.Add(ttl)}
}
