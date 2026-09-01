package musicbrainz

import (
	"sync"
	"time"
)

// ttlCache is an in-memory cache for expensive responses to the MusicBrainz API.
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
	// Re-check under the write lock before deleting, in case another goroutine updated
	// the entry in the like six lines since unlocking.
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

	// If .get() is never called on expired entries, they'll never get
	// deleted from the cache -- so good to take the opportunity afforded
	// by the lock to do a lil housekeeping.
	for key, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, key)
		}
	}

	// Still full after housekeeping? Evict whatever is closest to expiring
	// to make space for this entry.
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
