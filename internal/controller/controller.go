// Package controller is the cluster-level aggregator. It receives contention
// snapshots from every node's agent and presents a cluster-wide view.
//
// This is Phase 3, slice 1: observe-only. Each agent already does the per-node
// detection (offenders/victims/confidence); the controller's job here is to
// collect those judgements and show the whole cluster at a glance. Policy-driven
// decisions, Kubernetes events, and remediation come in later slices.
//
// It is portable Go (no eBPF), so it builds and runs anywhere.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/codecrafted007/node-sentinel/internal/report"
)

// nodeState is the latest snapshot from one node plus when it arrived.
type nodeState struct {
	Snapshot report.Snapshot `json:"snapshot"`
	LastSeen time.Time       `json:"last_seen"`
}

// Controller holds the latest report from every node.
type Controller struct {
	staleAfter time.Duration
	remediator *Remediator // nil = observe-only (issue #7)

	mu    sync.RWMutex
	nodes map[string]*nodeState
}

// New returns a controller. A node is considered stale (DataGap) if no report
// arrives within staleAfter.
func New(staleAfter time.Duration) *Controller {
	return &Controller{staleAfter: staleAfter, nodes: map[string]*nodeState{}}
}

// WithRemediator enables remediation: each ingested snapshot's confident
// offenders are acted on (issue #7). Without it the controller is observe-only.
func (c *Controller) WithRemediator(r *Remediator) *Controller {
	c.remediator = r
	return c
}

// ingest records a snapshot from a node and, if remediation is enabled, acts on
// its confident offenders.
func (c *Controller) ingest(ctx context.Context, s report.Snapshot, now time.Time) {
	name := s.NodeName
	if name == "" {
		name = "unknown"
	}
	c.mu.Lock()
	c.nodes[name] = &nodeState{Snapshot: s, LastSeen: now}
	c.mu.Unlock()

	if c.remediator != nil && !s.Healthy {
		c.remediator.Remediate(ctx, s)
	}
}

// Serve runs the HTTP API until ctx is cancelled:
//
//	POST /report   an agent posts its JSON snapshot
//	GET  /status   the whole-cluster view as JSON
//	GET  /healthz  liveness
func (c *Controller) Serve(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/report", c.handleReport)
	mux.HandleFunc("/status", c.handleStatus)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	return srv.ListenAndServe()
}

func (c *Controller) handleReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var s report.Snapshot
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		http.Error(w, "bad snapshot: "+err.Error(), http.StatusBadRequest)
		return
	}
	c.ingest(r.Context(), s, time.Now())
	w.WriteHeader(http.StatusNoContent)
}

func (c *Controller) handleStatus(w http.ResponseWriter, _ *http.Request) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(c.nodes)
}

// LogLoop prints a one-line cluster summary every interval until ctx is done.
func (c *Controller) LogLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.logCluster(time.Now())
		}
	}
}

func (c *Controller) logCluster(now time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	names := make([]string, 0, len(c.nodes))
	for n := range c.nodes {
		names = append(names, n)
	}
	sort.Strings(names)

	var healthy, contended, stale int
	type row struct{ name, detail string }
	var hot []row

	for _, n := range names {
		st := c.nodes[n]
		switch {
		case now.Sub(st.LastSeen) > c.staleAfter:
			stale++
		case st.Snapshot.Healthy:
			healthy++
		default:
			contended++
			hot = append(hot, row{n, summarize(st.Snapshot)})
		}
	}

	fmt.Printf("[cluster] %s  nodes=%d healthy=%d contended=%d stale=%d\n",
		now.Format("15:04:05"), len(names), healthy, contended, stale)
	for _, r := range hot {
		fmt.Printf("  %-24s %s\n", r.name, r.detail)
	}
}

// summarize renders one contended node's headline.
func summarize(s report.Snapshot) string {
	out := fmt.Sprintf("CONTENDED  CPU:%d I/O:%d NET:%d", len(s.Victims), len(s.IOVictims), len(s.NetVictims))

	maxConf := s.MaxConfidence
	if s.IOMaxConfidence > maxConf {
		maxConf = s.IOMaxConfidence
	}
	if s.NetMaxConfidence > maxConf {
		maxConf = s.NetMaxConfidence
	}
	if maxConf >= s.ConfidenceMin {
		out += fmt.Sprintf("  — confident offender (%.0f%%)", maxConf*100)
	} else {
		out += "  — alert only (no confident offender)"
	}
	return out
}
