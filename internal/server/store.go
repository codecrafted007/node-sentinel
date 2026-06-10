// Package server exposes the agent's snapshot over a Prometheus /metrics
// endpoint (for dashboards) and a unix socket (for sentinelctl). It is portable
// Go — no eBPF — so it builds and tests on any OS.
package server

import (
	"sync"

	"github.com/codecrafted007/node-sentinal/internal/report"
)

// Store holds the latest snapshot, written by the agent each interval and read
// by the metrics and local endpoints on demand. Decoupling it this way means
// the agent keeps its own read-and-delete cadence while scrapes/CLI reads are
// served from the cached snapshot.
type Store struct {
	mu   sync.RWMutex
	snap report.Snapshot
}

// NewStore returns an empty store.
func NewStore() *Store { return &Store{} }

// Set replaces the current snapshot.
func (s *Store) Set(snap report.Snapshot) {
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
}

// Get returns the latest snapshot.
func (s *Store) Get() report.Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap
}
