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
	"github.com/codecrafted007/node-sentinal/internal/report"
	"github.com/codecrafted007/node-sentinal/internal/server"
)

// Agent owns the node-agent lifecycle: load the scheduler observer, resolve
// cgroups to pods, judge contention each interval, and publish the result to
// stdout, the Prometheus endpoint, and the sentinelctl socket.
type Agent struct {
	cfg      Config
	sched    *ebpf.SchedObserver
	resolver *cgroup.Resolver
	store    *server.Store
}

// New constructs an Agent with the given config.
func New(cfg Config) *Agent { return &Agent{cfg: cfg} }

// Run loads the observer, starts the metrics + local servers, and reads the
// maps on the configured interval until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	sched, err := ebpf.LoadSched()
	if err != nil {
		return fmt.Errorf("load sched observer: %w", err)
	}
	a.sched = sched
	defer a.sched.Close()

	// Pod resolution is best-effort — the agent still runs if CRI is down.
	if res, err := cgroup.NewResolver(a.cfg.CRISocket, a.cfg.CgroupRoot); err != nil {
		fmt.Printf("warning: pod resolver disabled (%v); showing raw cgroup IDs\n", err)
	} else {
		a.resolver = res
		defer a.resolver.Close()
		fmt.Printf("pod resolver: %d containers mapped\n", res.Len())
	}

	a.store = server.NewStore()
	if a.cfg.MetricsAddr != "" {
		addr := a.cfg.MetricsAddr
		go func() {
			if err := server.ServeMetrics(ctx, addr, a.store); err != nil && ctx.Err() == nil {
				fmt.Printf("metrics server error: %v\n", err)
			}
		}()
		fmt.Printf("prometheus metrics: http://%s/metrics\n", addr)
	}
	if a.cfg.LocalSocket != "" {
		sock := a.cfg.LocalSocket
		go func() {
			if err := server.ServeLocal(ctx, sock, a.store); err != nil && ctx.Err() == nil {
				fmt.Printf("local server error: %v\n", err)
			}
		}()
		fmt.Printf("sentinelctl socket: %s\n", sock)
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

// report reads both signals, builds a snapshot, publishes it, and prints it.
func (a *Agent) report() error {
	cpu, err := a.sched.ReadCPU()
	if err != nil {
		return err
	}
	runq, err := a.sched.Read()
	if err != nil {
		return err
	}

	snap := a.buildSnapshot(cpu, runq)
	if a.store != nil {
		a.store.Set(snap)
	}
	a.printSnapshot(snap)
	return nil
}

// buildSnapshot turns the raw signals into a judgement: healthy unless at least
// one pod is genuinely starved of CPU, with offenders ranked and judged against
// their fair share.
func (a *Agent) buildSnapshot(cpu []ebpf.CgroupCPUTime, runq []ebpf.CgroupLatency) report.Snapshot {
	snap := report.Snapshot{
		Time:        time.Now().Format("15:04:05"),
		CgroupsSeen: len(runq),
		RunqWarnUs:  float64(a.cfg.RunqWarn.Microseconds()),
		MinSamples:  a.cfg.MinSamples,
	}

	victims := a.victims(runq)
	if len(victims) == 0 {
		snap.Healthy = true
		return snap
	}

	var totalNs uint64
	var totalReq int64
	for _, r := range cpu {
		totalNs += r.OnCpuNs
		if pod, ok := a.resolve(r.CgroupID); ok {
			totalReq += pod.RequestMilliCPU
		}
	}

	sort.Slice(cpu, func(i, j int) bool { return cpu[i].OnCpuNs > cpu[j].OnCpuNs })
	if len(cpu) > a.cfg.TopN {
		cpu = cpu[:a.cfg.TopN]
	}
	for _, r := range cpu {
		intensity := 0.0
		if totalNs > 0 {
			intensity = float64(r.OnCpuNs) / float64(totalNs) * 100
		}
		o := report.Offender{
			Pod:       a.label(r.CgroupID),
			CPUms:     float64(r.OnCpuNs) / 1e6,
			Intensity: intensity,
			ReqMilli:  -1,
			Verdict:   "system / unattributed",
		}
		if pod, ok := a.resolve(r.CgroupID); ok {
			o.ReqMilli = pod.RequestMilliCPU
			o.Verdict = fairShareVerdict(intensity, pod.RequestMilliCPU, totalReq)
		}
		snap.Offenders = append(snap.Offenders, o)
	}

	for _, r := range victims {
		snap.Victims = append(snap.Victims, report.Victim{
			Pod:    a.label(r.CgroupID),
			P50us:  metrics.Percentile(r.Slots, 50),
			P99us:  metrics.Percentile(r.Slots, 99),
			Events: r.Count,
		})
	}
	return snap
}

// victims returns cgroups whose run-queue latency is genuinely bad: enough
// samples to trust the percentile, and a p99 above the warning threshold.
func (a *Agent) victims(rows []ebpf.CgroupLatency) []ebpf.CgroupLatency {
	threshold := float64(a.cfg.RunqWarn.Microseconds())

	var out []ebpf.CgroupLatency
	for _, r := range rows {
		if r.Count < uint64(a.cfg.MinSamples) {
			continue // too few samples for a meaningful p99
		}
		if metrics.Percentile(r.Slots, 99) < threshold {
			continue // normal scheduling latency, not contention
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return metrics.Percentile(out[i].Slots, 99) > metrics.Percentile(out[j].Slots, 99)
	})
	if len(out) > a.cfg.TopN {
		out = out[:a.cfg.TopN]
	}
	return out
}

// fairShareVerdict compares a cgroup's CPU intensity to the share implied by its
// CPU request. A pod consuming beyond its fair share is the real offender.
func fairShareVerdict(intensity float64, reqMilli, totalReqMilli int64) string {
	if reqMilli <= 0 {
		return "no request (best-effort)"
	}
	if totalReqMilli <= 0 {
		return "no request data"
	}
	fair := float64(reqMilli) / float64(totalReqMilli) * 100
	if intensity > fair {
		return fmt.Sprintf("OVER fair share (%.1f%%)", fair)
	}
	return fmt.Sprintf("within request (%.1f%%)", fair)
}

// printSnapshot renders a snapshot to stdout: a one-line heartbeat when healthy,
// the offender/victim tables when contended.
func (a *Agent) printSnapshot(s report.Snapshot) {
	if s.Healthy {
		fmt.Printf("%s  [OK] healthy — no CPU contention (no pod above run-queue p99 %s with >=%d samples; %d cgroups seen)\n",
			s.Time, a.cfg.RunqWarn, s.MinSamples, s.CgroupsSeen)
		return
	}

	fmt.Printf("\n%s  [!] CPU CONTENTION — %d pod(s) starved on the run queue\n", s.Time, len(s.Victims))

	fmt.Printf("  OFFENDERS — by CPU time\n")
	fmt.Printf("  %-42s %10s %10s %9s  %s\n", "POD", "CPU_MS", "INTENSITY", "REQ_mCPU", "VERDICT")
	for _, o := range s.Offenders {
		req := "-"
		if o.ReqMilli >= 0 {
			req = fmt.Sprintf("%d", o.ReqMilli)
		}
		fmt.Printf("  %-42s %10.0f %9.1f%% %9s  %s\n", truncate(o.Pod, 42), o.CPUms, o.Intensity, req, o.Verdict)
	}

	fmt.Printf("  VICTIMS — by run-queue latency (p99 >= %s, >=%d samples)\n", a.cfg.RunqWarn, a.cfg.MinSamples)
	fmt.Printf("  %-42s %12s %12s %10s\n", "POD", "RUNQ_P50_US", "RUNQ_P99_US", "EVENTS")
	for _, v := range s.Victims {
		fmt.Printf("  %-42s %12.0f %12.0f %10d\n", truncate(v.Pod, 42), v.P50us, v.P99us, v.Events)
	}
}

// resolve returns the full pod identity (including CPU request) for a cgroup.
func (a *Agent) resolve(cgroupID uint64) (cgroup.PodID, bool) {
	if a.resolver == nil {
		return cgroup.PodID{}, false
	}
	return a.resolver.Resolve(cgroupID)
}

// label renders a cgroup's pod identity, or a system fallback.
func (a *Agent) label(cgroupID uint64) string {
	if pod, ok := a.resolve(cgroupID); ok {
		return pod.String()
	}
	return fmt.Sprintf("system(cg:%d)", cgroupID)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
