package cgroup

import (
	"testing"
	"time"
)

func podOf(ns string) PodID { return PodID{Namespace: ns, Pod: "p", Container: "c"} }

// a ttlCache with a controllable clock.
func newTestCache(ttl time.Duration) (*ttlCache, *time.Time) {
	c := newTTLCache(ttl)
	clock := time.Unix(1_000_000, 0)
	c.now = func() time.Time { return clock }
	return c, &clock
}

func TestTTLCacheLiveEntriesNeverExpire(t *testing.T) {
	c, clock := newTestCache(30 * time.Second)
	c.replace(map[uint64]PodID{1: podOf("a"), 2: podOf("b")})

	*clock = clock.Add(time.Hour) // long past any TTL
	c.replace(map[uint64]PodID{1: podOf("a"), 2: podOf("b")}) // both still live
	if _, ok := c.get(1); !ok {
		t.Error("live entry 1 must not expire")
	}
	if _, ok := c.get(2); !ok {
		t.Error("live entry 2 must not expire")
	}
}

func TestTTLCacheVanishedEntrySurvivesGracePeriod(t *testing.T) {
	c, clock := newTestCache(30 * time.Second)
	c.replace(map[uint64]PodID{1: podOf("a"), 2: podOf("b")})

	// cgroup 2 vanishes — its name must still resolve during the grace period.
	*clock = clock.Add(5 * time.Second)
	c.replace(map[uint64]PodID{1: podOf("a")})
	if pod, ok := c.get(2); !ok || pod.Namespace != "b" {
		t.Fatalf("vanished entry dropped too early: got %v ok=%v", pod, ok)
	}

	// still within TTL on a later refresh — deadline does NOT reset, counts from
	// when it first went absent.
	*clock = clock.Add(20 * time.Second) // 25s since vanish, < 30s
	c.replace(map[uint64]PodID{1: podOf("a")})
	if _, ok := c.get(2); !ok {
		t.Error("entry expired before its grace period")
	}

	// past TTL — now it ages out (vanished at +5s, expiry +35s; this is +36s).
	*clock = clock.Add(11 * time.Second)
	c.replace(map[uint64]PodID{1: podOf("a")})
	if _, ok := c.get(2); ok {
		t.Error("entry should have aged out after the grace period")
	}
}

func TestTTLCacheGetRejectsExpiredBeforePrune(t *testing.T) {
	c, clock := newTestCache(10 * time.Second)
	c.replace(map[uint64]PodID{1: podOf("a")})
	c.replace(map[uint64]PodID{}) // 1 goes absent, expiry = now+10s

	*clock = clock.Add(11 * time.Second) // past expiry, but no replace() to prune
	if _, ok := c.get(1); ok {
		t.Error("get must reject an expired tombstone even before pruning")
	}
}

func TestTTLCacheTombstone(t *testing.T) {
	c, clock := newTestCache(30 * time.Second)
	c.replace(map[uint64]PodID{1: podOf("a")})

	c.tombstone(1) // rmdir observed — start the grace period immediately
	if _, ok := c.get(1); !ok {
		t.Error("tombstoned entry must still resolve during grace")
	}
	*clock = clock.Add(31 * time.Second)
	if _, ok := c.get(1); ok {
		t.Error("tombstoned entry must age out after TTL")
	}
}

func TestTTLCachePutAddsLive(t *testing.T) {
	c, clock := newTestCache(5 * time.Second)
	c.put(7, podOf("z")) // lazy join from the watcher, no full scan
	*clock = clock.Add(time.Hour)
	if pod, ok := c.get(7); !ok || pod.Namespace != "z" {
		t.Errorf("put entry should be live and resolve: got %v ok=%v", pod, ok)
	}
}
