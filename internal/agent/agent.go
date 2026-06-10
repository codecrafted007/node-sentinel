//go:build linux

package agent

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/codecrafted007/node-sentinal/internal/cgroup"
	"github.com/codecrafted007/node-sentinal/internal/ebpf"
	"github.com/codecrafted007/node-sentinal/internal/metrics"
)

// Agent owns the node-agent lifecycle. Phase 1: load the scheduler observer,
// resolve cgroups to pods, and print per-pod run-queue latency on an interval.
type Agent struct {
	cfg      Config
	sched    *ebpf.SchedObserver
	resolver *cgroup.Resolver
}

// New constructs an Agent with the given config.
func New(cfg Config) *Agent { return &Agent{cfg: cfg} }

// Run loads the observer, attaches the pod resolver, and reads the maps on the
// configured interval until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	sched, err := ebpf.LoadSched()
	if err != nil {
		return fmt.Errorf("load sched observer: %w", err)
	}
	a.sched = sched
	defer a.sched.Close()

	// Pod resolution is optional — if CRI is unreachable the agent still runs
	// and prints raw cgroup IDs (design: never block observability on the CRI).
	if res, err := cgroup.NewResolver(a.cfg.CRISocket, a.cfg.CgroupRoot); err != nil {
		fmt.Printf("warning: pod resolver disabled (%v); showing raw cgroup IDs\n", err)
	} else {
		a.resolver = res
		defer a.resolver.Close()
		fmt.Printf("pod resolver: %d containers mapped\n", res.Len())
	}

	fmt.Printf("node-sentinel agent: sched observer attached, reading every %s\n", a.cfg.ReadInterval)

	readT := time.NewTicker(a.cfg.ReadInterval)
	defer readT.Stop()
	refreshT := time.NewTicker(a.cfg.ResolveRefresh)
	defer refreshT.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-refreshT.C:
			if a.resolver != nil {
				if err := a.resolver.Refresh(ctx); err != nil {
					fmt.Printf("resolver refresh error: %v\n", err)
				}
			}
		case <-readT.C:
			if err := a.report(); err != nil {
				fmt.Printf("read error: %v\n", err)
			}
		}
	}
}

// report reads the run-queue latency map and prints the worst cgroups by p99,
// labelled with pod identity where resolvable.
func (a *Agent) report() error {
	rows, err := a.sched.Read()
	if err != nil {
		return err
	}

	// Highest run-queue p99 first — the likely victims/offenders.
	sort.Slice(rows, func(i, j int) bool {
		return metrics.Percentile(rows[i].Slots, 99) > metrics.Percentile(rows[j].Slots, 99)
	})
	if len(rows) > a.cfg.TopN {
		rows = rows[:a.cfg.TopN]
	}

	fmt.Printf("\n%-44s %12s %12s %10s\n", "POD (namespace/pod/container)", "RUNQ_P50_US", "RUNQ_P99_US", "EVENTS")
	for _, r := range rows {
		fmt.Printf("%-44s %12.0f %12.0f %10d\n",
			a.label(r.CgroupID),
			metrics.Percentile(r.Slots, 50),
			metrics.Percentile(r.Slots, 99),
			r.Count,
		)
	}
	return nil
}

// label renders a cgroup's pod identity, or a system fallback for cgroups not
// backed by a CRI container.
func (a *Agent) label(cgroupID uint64) string {
	if a.resolver != nil {
		if pod, ok := a.resolver.Resolve(cgroupID); ok {
			return truncate(pod.String(), 44)
		}
	}
	return fmt.Sprintf("system(cg:%d)", cgroupID)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
