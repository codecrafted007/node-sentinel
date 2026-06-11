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
	baseline *metrics.Baseline
	store    *server.Store
}

// New constructs an Agent with the given config.
func New(cfg Config) *Agent {
	return &Agent{
		cfg:      cfg,
		baseline: metrics.NewBaseline(cfg.BaselineAlpha, cfg.BaselineWarmup),
	}
}

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

// buildSnapshot turns the raw signals into a judgement. Victims are pods whose
// run-queue latency is genuinely bad (absolute floor — the primary, restart-safe
// signal) and, once their baseline is warm, unusual *for themselves*. Offenders
// are ranked by CPU and judged against their fair share, with a confidence score
// that combines how far they exceed their share and how badly victims degraded.
func (a *Agent) buildSnapshot(cpu []ebpf.CgroupCPUTime, runq []ebpf.CgroupLatency) report.Snapshot {
	snap := report.Snapshot{
		Time:          time.Now().Format("15:04:05"),
		CgroupsSeen:   len(runq),
		RunqWarnUs:    float64(a.cfg.RunqWarn.Microseconds()),
		MinSamples:    a.cfg.MinSamples,
		ConfidenceMin: a.cfg.ConfidenceThreshold,
		MaxConfidence: -1,
	}

	floor := float64(a.cfg.RunqWarn.Microseconds())

	type vrow struct {
		cg     uint64
		p50    float64
		p99    float64
		ratio  float64 // current / baseline; 0 if baseline not warm
		events uint64
	}
	var victims []vrow
	worstRatio := 0.0

	for _, r := range runq {
		if r.Count < uint64(a.cfg.MinSamples) {
			continue // too few samples for a meaningful p99
		}
		p99 := metrics.Percentile(r.Slots, 99)
		ratio, ready := a.baseline.Deviation(r.CgroupID, p99)

		isVictim := p99 >= floor
		if ready && ratio < a.cfg.DeviationFactor {
			isVictim = false // warm and not unusual for itself — it's just always like this
		}

		// Learn its normal, but freeze while it's a known (warm) victim so a
		// sustained spike isn't absorbed into "normal".
		a.baseline.Observe(r.CgroupID, p99, !(ready && isVictim))

		if isVictim {
			v := vrow{cg: r.CgroupID, p50: metrics.Percentile(r.Slots, 50), p99: p99, events: r.Count}
			if ready {
				v.ratio = ratio
				if ratio > worstRatio {
					worstRatio = ratio
				}
			}
			victims = append(victims, v)
		}
	}
	a.baseline.Prune()

	if len(victims) == 0 {
		snap.Healthy = true
		return snap
	}

	// How badly are victims degraded? Drives the victim side of confidence.
	// With no warm victim yet we only have the absolute signal — use a moderate
	// default rather than overclaiming.
	victimSignal := 0.5
	if worstRatio > 1 {
		victimSignal = clamp((worstRatio-1)/4, 0, 1) // 5x its baseline → full
	}

	snap.Offenders = a.offenders(cpu, victimSignal, &snap.MaxConfidence)

	sort.Slice(victims, func(i, j int) bool { return victims[i].p99 > victims[j].p99 })
	if len(victims) > a.cfg.TopN {
		victims = victims[:a.cfg.TopN]
	}
	for _, v := range victims {
		snap.Victims = append(snap.Victims, report.Victim{
			Pod:         a.label(v.cg),
			P50us:       v.p50,
			P99us:       v.p99,
			Degradation: v.ratio,
			Events:      v.events,
		})
	}
	return snap
}

// offenders ranks cgroups by CPU time and scores each attributable pod's
// confidence of being the noisy neighbour. victimSignal (0-1) is how badly the
// victims are degraded; it caps confidence so we only blame a pod when there is
// real harm. maxConf is updated with the highest confidence seen.
func (a *Agent) offenders(cpu []ebpf.CgroupCPUTime, victimSignal float64, maxConf *float64) []report.Offender {
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

	out := make([]report.Offender, 0, len(cpu))
	for _, r := range cpu {
		intensity := 0.0
		if totalNs > 0 {
			intensity = float64(r.OnCpuNs) / float64(totalNs) * 100
		}
		o := report.Offender{
			Pod:        a.label(r.CgroupID),
			CPUms:      float64(r.OnCpuNs) / 1e6,
			Intensity:  intensity,
			ReqMilli:   -1,
			Confidence: -1,
			Verdict:    "system / unattributed",
		}
		if pod, ok := a.resolve(r.CgroupID); ok {
			o.ReqMilli = pod.RequestMilliCPU
			o.Verdict = fairShareVerdict(intensity, pod.RequestMilliCPU, totalReq)
			if pod.RequestMilliCPU > 0 && totalReq > 0 {
				fair := float64(pod.RequestMilliCPU) / float64(totalReq) * 100
				excessFrac := (intensity - fair) / 100 // fraction of node CPU over fair share
				offenderSignal := clamp(excessFrac/0.5, 0, 1)
				o.Confidence = clamp(min(offenderSignal, victimSignal), 0, 1)
				if o.Confidence > *maxConf {
					*maxConf = o.Confidence
				}
			}
		}
		out = append(out, o)
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
// the offender/victim tables plus an attribution summary when contended.
func (a *Agent) printSnapshot(s report.Snapshot) {
	if s.Healthy {
		fmt.Printf("%s  [OK] healthy — no CPU contention (no pod above run-queue p99 %s with >=%d samples; %d cgroups seen)\n",
			s.Time, a.cfg.RunqWarn, s.MinSamples, s.CgroupsSeen)
		return
	}

	fmt.Printf("\n%s  [!] CPU CONTENTION — %d pod(s) starved on the run queue\n", s.Time, len(s.Victims))
	fmt.Printf("  %s\n", attribution(s))

	fmt.Printf("  OFFENDERS — by CPU time\n")
	fmt.Printf("  %-42s %9s %9s %7s %10s  %s\n", "POD", "CPU_MS", "INTENSITY", "REQ_m", "CONFIDENCE", "VERDICT")
	for _, o := range s.Offenders {
		fmt.Printf("  %-42s %9.0f %8.1f%% %7s %10s  %s\n",
			truncate(o.Pod, 42), o.CPUms, o.Intensity, reqStr(o.ReqMilli), confStr(o.Confidence), o.Verdict)
	}

	fmt.Printf("  VICTIMS — by run-queue latency (p99 >= %s, >=%d samples)\n", a.cfg.RunqWarn, a.cfg.MinSamples)
	fmt.Printf("  %-42s %12s %12s %9s %10s\n", "POD", "RUNQ_P50_US", "RUNQ_P99_US", "xBASELINE", "EVENTS")
	for _, v := range s.Victims {
		fmt.Printf("  %-42s %12.0f %12.0f %9s %10d\n",
			truncate(v.Pod, 42), v.P50us, v.P99us, degStr(v.Degradation), v.Events)
	}
}

// attribution summarizes whether any pod is a confident offender.
func attribution(s report.Snapshot) string {
	switch {
	case s.MaxConfidence < 0:
		return "attribution: top consumer is unattributed (likely a system process) — no pod offender"
	case s.MaxConfidence >= s.ConfidenceMin:
		return fmt.Sprintf("attribution: confident pod offender (%.0f%% >= %.0f%% threshold)",
			s.MaxConfidence*100, s.ConfidenceMin*100)
	default:
		return fmt.Sprintf("attribution: low confidence (%.0f%% < %.0f%% threshold) — alert only, no clear pod offender",
			s.MaxConfidence*100, s.ConfidenceMin*100)
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

func clamp(x, lo, hi float64) float64 { return min(hi, max(lo, x)) }

func reqStr(m int64) string {
	if m < 0 {
		return "-"
	}
	return fmt.Sprintf("%d", m)
}

func confStr(c float64) string {
	if c < 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", c*100)
}

func degStr(r float64) string {
	if r <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1fx", r)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
