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

// Agent owns the node-agent lifecycle: load the observers, resolve cgroups to
// pods, judge contention (CPU and disk I/O) each interval, and publish the
// result to stdout, the Prometheus endpoint, and the sentinelctl socket.
type Agent struct {
	cfg        Config
	sched      *ebpf.SchedObserver
	blkio      *ebpf.BlkioObserver
	resolver   *cgroup.Resolver
	baseline   *metrics.Baseline // run-queue latency normals
	ioBaseline *metrics.Baseline // I/O latency normals
	store      *server.Store
}

// New constructs an Agent with the given config.
func New(cfg Config) *Agent {
	return &Agent{
		cfg:        cfg,
		baseline:   metrics.NewBaseline(cfg.BaselineAlpha, cfg.BaselineWarmup),
		ioBaseline: metrics.NewBaseline(cfg.BaselineAlpha, cfg.BaselineWarmup),
	}
}

// Run loads the observers, starts the metrics + local servers, and reads the
// maps on the configured interval until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	sched, err := ebpf.LoadSched()
	if err != nil {
		return fmt.Errorf("load sched observer: %w", err)
	}
	a.sched = sched
	defer a.sched.Close()

	// Block-I/O observer is best-effort — if it fails to load the agent still
	// runs with CPU contention detection.
	if blk, err := ebpf.LoadBlkio(); err != nil {
		fmt.Printf("warning: blkio observer disabled (%v)\n", err)
	} else {
		a.blkio = blk
		defer a.blkio.Close()
		fmt.Printf("blkio observer attached\n")
	}

	// Pod resolution is best-effort — the agent still runs if CRI is down.
	if res, err := cgroup.NewResolver(a.cfg.CRISocket, a.cfg.CgroupRoot); err != nil {
		fmt.Printf("warning: pod resolver disabled (%v); showing raw cgroup IDs\n", err)
	} else {
		a.resolver = res
		defer a.resolver.Close()
		fmt.Printf("pod resolver: %d containers mapped\n", res.Len())

		// Live cgroup updates: refresh the moment pods come/go, instead of only
		// on the periodic rescan (design §7.4). The rescan stays the safety net.
		if w, err := cgroup.NewWatcher(a.cfg.CgroupRoot); err != nil {
			fmt.Printf("warning: cgroup watcher disabled (%v); periodic rescan only\n", err)
		} else {
			go w.Run(ctx, func() { _ = a.resolver.Refresh(ctx) })
			fmt.Printf("cgroup watcher: live pod updates enabled\n")
		}
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

	fmt.Printf("node-sentinel agent: observers attached, reading every %s\n", a.cfg.ReadInterval)

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

// report reads every signal, builds a snapshot, publishes it, and prints it.
func (a *Agent) report() error {
	cpu, err := a.sched.ReadCPU()
	if err != nil {
		return err
	}
	runq, err := a.sched.Read()
	if err != nil {
		return err
	}
	var blkio []ebpf.CgroupBlkio
	if a.blkio != nil {
		if io, err := a.blkio.Read(); err == nil {
			blkio = io
		}
	}

	snap := a.buildSnapshot(cpu, runq, blkio)
	if a.store != nil {
		a.store.Set(snap)
	}
	a.printSnapshot(snap)
	return nil
}

// buildSnapshot judges both dimensions. The node is healthy unless at least one
// pod is genuinely starved of CPU or disk I/O.
func (a *Agent) buildSnapshot(cpu []ebpf.CgroupCPUTime, runq []ebpf.CgroupLatency, blkio []ebpf.CgroupBlkio) report.Snapshot {
	snap := report.Snapshot{
		Time:            time.Now().Format("15:04:05"),
		CgroupsSeen:     len(runq),
		RunqWarnUs:      float64(a.cfg.RunqWarn.Microseconds()),
		MinSamples:      a.cfg.MinSamples,
		ConfidenceMin:   a.cfg.ConfidenceThreshold,
		MaxConfidence:   -1,
		IOMaxConfidence: -1,
	}

	cpuVictims, cpuWorst := a.cpuVictims(runq)
	ioVictims, ioWorst := a.ioVictims(blkio)

	snap.Healthy = len(cpuVictims) == 0 && len(ioVictims) == 0
	if snap.Healthy {
		return snap
	}

	if len(cpuVictims) > 0 {
		snap.Offenders = a.cpuOffenders(cpu, signalFromRatio(cpuWorst), &snap.MaxConfidence)
		snap.Victims = cpuVictims
	}
	if len(ioVictims) > 0 {
		snap.IOOffenders = a.ioOffenders(blkio, signalFromRatio(ioWorst), &snap.IOMaxConfidence)
		snap.IOVictims = ioVictims
	}
	return snap
}

// cpuVictims returns the pods whose run-queue latency is genuinely bad (above
// the absolute floor and, once their baseline is warm, unusual for themselves),
// plus the worst degradation ratio seen.
func (a *Agent) cpuVictims(runq []ebpf.CgroupLatency) ([]report.Victim, float64) {
	out, worst := a.victims(a.baseline, float64(a.cfg.RunqWarn.Microseconds()), a.cfg.MinSamples,
		latencyRows(runq))
	return out, worst
}

// ioVictims is the I/O analogue of cpuVictims.
func (a *Agent) ioVictims(blkio []ebpf.CgroupBlkio) ([]report.Victim, float64) {
	rows := make([]latencyRow, 0, len(blkio))
	for _, r := range blkio {
		rows = append(rows, latencyRow{cg: r.CgroupID, slots: r.Slots, samples: r.Count})
	}
	return a.victims(a.ioBaseline, float64(a.cfg.IOWarn.Microseconds()), a.cfg.MinOps, rows)
}

// latencyRow is a generic per-cgroup latency histogram used by victims().
type latencyRow struct {
	cg      uint64
	slots   []uint64
	samples uint64
}

func latencyRows(runq []ebpf.CgroupLatency) []latencyRow {
	rows := make([]latencyRow, 0, len(runq))
	for _, r := range runq {
		rows = append(rows, latencyRow{cg: r.CgroupID, slots: r.Slots, samples: r.Count})
	}
	return rows
}

// victims runs the shared victim judgement over any latency dimension: enough
// samples, above the absolute floor, and — once the per-cgroup baseline is warm
// — at least DeviationFactor times its own normal. Returns the victims (worst
// p99 first, capped at TopN) and the worst degradation ratio.
func (a *Agent) victims(baseline *metrics.Baseline, floorUs float64, minSamples int, rows []latencyRow) ([]report.Victim, float64) {
	var out []report.Victim
	worst := 0.0

	for _, r := range rows {
		if r.samples < uint64(minSamples) {
			continue
		}
		p99 := metrics.Percentile(r.slots, 99)
		ratio, ready := baseline.Deviation(r.cg, p99)

		isVictim := p99 >= floorUs
		if ready && ratio < a.cfg.DeviationFactor {
			isVictim = false // warm and not unusual for itself
		}
		baseline.Observe(r.cg, p99, !(ready && isVictim))

		if isVictim {
			deg := 0.0
			if ready {
				deg = ratio
				if ratio > worst {
					worst = ratio
				}
			}
			out = append(out, report.Victim{
				Pod:         a.label(r.cg),
				P50us:       metrics.Percentile(r.slots, 50),
				P99us:       p99,
				Degradation: deg,
				Events:      r.samples,
			})
		}
	}
	baseline.Prune()

	sort.Slice(out, func(i, j int) bool { return out[i].P99us > out[j].P99us })
	if len(out) > a.cfg.TopN {
		out = out[:a.cfg.TopN]
	}
	return out, worst
}

// signalFromRatio maps a worst-victim degradation ratio to a 0-1 severity used
// to cap offender confidence. With no warm victim we only have the absolute
// signal, so use a moderate default rather than overclaiming.
func signalFromRatio(worst float64) float64 {
	if worst > 1 {
		return clamp((worst-1)/4, 0, 1) // 5x its baseline → full
	}
	return 0.5
}

// cpuOffenders ranks cgroups by CPU time and scores each attributable pod's
// confidence of being the noisy neighbour, capped by how badly victims degraded.
func (a *Agent) cpuOffenders(cpu []ebpf.CgroupCPUTime, victimSignal float64, maxConf *float64) []report.Offender {
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
				excessFrac := (intensity - fair) / 100
				o.Confidence = clamp(min(clamp(excessFrac/0.5, 0, 1), victimSignal), 0, 1)
				if o.Confidence > *maxConf {
					*maxConf = o.Confidence
				}
			}
		}
		out = append(out, o)
	}
	return out
}

// ioOffenders ranks cgroups by disk throughput and scores each attributable
// pod's confidence from its share of disk bytes, capped by victim severity.
func (a *Agent) ioOffenders(blkio []ebpf.CgroupBlkio, victimSignal float64, maxConf *float64) []report.IOOffender {
	var totalBytes uint64
	for _, r := range blkio {
		totalBytes += r.Bytes
	}

	sort.Slice(blkio, func(i, j int) bool { return blkio[i].Bytes > blkio[j].Bytes })
	if len(blkio) > a.cfg.TopN {
		blkio = blkio[:a.cfg.TopN]
	}

	out := make([]report.IOOffender, 0, len(blkio))
	for _, r := range blkio {
		share := 0.0
		if totalBytes > 0 {
			share = float64(r.Bytes) / float64(totalBytes) * 100
		}
		o := report.IOOffender{
			Pod:        a.label(r.CgroupID),
			MB:         float64(r.Bytes) / 1e6,
			SharePct:   share,
			Ops:        r.Count,
			Confidence: -1,
		}
		if _, ok := a.resolve(r.CgroupID); ok {
			o.Confidence = clamp(min(clamp(share/100/0.5, 0, 1), victimSignal), 0, 1)
			if o.Confidence > *maxConf {
				*maxConf = o.Confidence
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
// per-dimension offender/victim tables when contended.
func (a *Agent) printSnapshot(s report.Snapshot) {
	if s.Healthy {
		fmt.Printf("%s  [OK] healthy — no contention (CPU + disk I/O nominal; %d cgroups seen)\n", s.Time, s.CgroupsSeen)
		return
	}

	fmt.Printf("\n%s  [!] CONTENTION — CPU: %d victim(s), I/O: %d victim(s)\n", s.Time, len(s.Victims), len(s.IOVictims))

	if len(s.Victims) > 0 {
		fmt.Printf("  ── CPU ──  %s\n", attribution(s.MaxConfidence, s.ConfidenceMin))
		fmt.Printf("  OFFENDERS — by CPU time\n")
		fmt.Printf("  %-42s %9s %9s %7s %10s  %s\n", "POD", "CPU_MS", "INTENSITY", "REQ_m", "CONFIDENCE", "VERDICT")
		for _, o := range s.Offenders {
			fmt.Printf("  %-42s %9.0f %8.1f%% %7s %10s  %s\n",
				truncate(o.Pod, 42), o.CPUms, o.Intensity, reqStr(o.ReqMilli), confStr(o.Confidence), o.Verdict)
		}
		a.printVictims("run-queue latency", s.Victims)
	}

	if len(s.IOVictims) > 0 {
		fmt.Printf("  ── DISK I/O ──  %s\n", attribution(s.IOMaxConfidence, s.ConfidenceMin))
		fmt.Printf("  OFFENDERS — by disk throughput\n")
		fmt.Printf("  %-42s %10s %9s %8s %10s\n", "POD", "MB", "SHARE", "OPS", "CONFIDENCE")
		for _, o := range s.IOOffenders {
			fmt.Printf("  %-42s %10.1f %8.1f%% %8d %10s\n",
				truncate(o.Pod, 42), o.MB, o.SharePct, o.Ops, confStr(o.Confidence))
		}
		a.printVictims("I/O latency", s.IOVictims)
	}
}

func (a *Agent) printVictims(metric string, rows []report.Victim) {
	fmt.Printf("  VICTIMS — by %s\n", metric)
	fmt.Printf("  %-42s %12s %12s %9s %10s\n", "POD", "P50_US", "P99_US", "xBASELINE", "EVENTS")
	for _, v := range rows {
		fmt.Printf("  %-42s %12.0f %12.0f %9s %10d\n",
			truncate(v.Pod, 42), v.P50us, v.P99us, degStr(v.Degradation), v.Events)
	}
}

// attribution summarizes whether any pod is a confident offender for a dimension.
func attribution(maxConf, threshold float64) string {
	switch {
	case maxConf < 0:
		return "attribution: top consumer is unattributed (likely a system process) — no pod offender"
	case maxConf >= threshold:
		return fmt.Sprintf("attribution: confident pod offender (%.0f%% >= %.0f%% threshold)", maxConf*100, threshold*100)
	default:
		return fmt.Sprintf("attribution: low confidence (%.0f%% < %.0f%% threshold) — alert only", maxConf*100, threshold*100)
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
