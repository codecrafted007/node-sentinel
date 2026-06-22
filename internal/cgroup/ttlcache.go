package cgroup

import (
	"sync"
	"time"
)

// ttlCache holds cgroup_id -> PodID with a grace period (issue #3). When a
// cgroup disappears its name is NOT dropped immediately: it is kept for `ttl` so
// a histogram captured in the cgroup's final interval still resolves to a real
// pod instead of `unknown`. Live entries never expire; an entry only ages out
// once it has been absent (or tombstoned) for longer than ttl.
//
// Safe for concurrent use. The clock is injectable for tests.
type ttlCache struct {
	ttl time.Duration
	now func() time.Time

	mu    sync.RWMutex
	items map[uint64]ttlEntry
}

type ttlEntry struct {
	pod PodID
	// expiry is zero while the cgroup is live; once it goes absent it is set to
	// the moment the grace period ends.
	expiry time.Time
}

func newTTLCache(ttl time.Duration) *ttlCache {
	return &ttlCache{ttl: ttl, now: time.Now, items: map[uint64]ttlEntry{}}
}

// get returns the pod for a cgroup if it is live or still within its grace
// period. An expired tombstone not yet pruned by replace reads as absent.
func (c *ttlCache) get(id uint64) (PodID, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.items[id]
	if !ok {
		return PodID{}, false
	}
	if !e.expiry.IsZero() && c.now().After(e.expiry) {
		return PodID{}, false
	}
	return e.pod, true
}

// replace merges a fresh scan of the currently-live cgroups: live entries are
// (re)added and marked live, entries no longer live keep their name until their
// grace period elapses, and expired entries are pruned. Call once per Refresh.
func (c *ttlCache) replace(live map[uint64]PodID) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()

	// Any entry that was live but is missing from this scan starts its grace
	// period now (entries already counting down keep their original deadline).
	for id, e := range c.items {
		if e.expiry.IsZero() {
			e.expiry = now.Add(c.ttl)
			c.items[id] = e
		}
	}
	// Revive everything still live (zero expiry = live).
	for id, pod := range live {
		c.items[id] = ttlEntry{pod: pod}
	}
	// Drop entries whose grace period has passed.
	for id, e := range c.items {
		if !e.expiry.IsZero() && now.After(e.expiry) {
			delete(c.items, id)
		}
	}
}

// put records a single cgroup_id -> pod binding as live, without a full scan —
// used by the lifecycle watcher's lazy CRI join on cgroup creation (issue #2).
func (c *ttlCache) put(id uint64, pod PodID) {
	c.mu.Lock()
	c.items[id] = ttlEntry{pod: pod}
	c.mu.Unlock()
}

// tombstone marks a cgroup absent now, starting its grace period — used when a
// cgroup_rmdir is observed (issue #2) so the name survives just long enough for
// the final-interval stats. A no-op if the id is unknown or already counting down.
func (c *ttlCache) tombstone(id uint64) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[id]; ok && e.expiry.IsZero() {
		e.expiry = now.Add(c.ttl)
		c.items[id] = e
	}
}

// len reports how many entries are currently held (live + within grace).
func (c *ttlCache) len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
