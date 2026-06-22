//go:build linux

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/codecrafted007/node-sentinel/internal/cgroup"
	"github.com/codecrafted007/node-sentinel/internal/ebpf"
	"github.com/codecrafted007/node-sentinel/internal/metrics"
	"github.com/codecrafted007/node-sentinel/internal/report"
	"github.com/codecrafted007/node-sentinel/internal/server"
)

// Agent owns the node-agent lifecycle: load the observers, resolve cgroups to
// pods, judge contention (CPU, disk I/O, network) each interval, and publish the
// result to stdout, the Prometheus endpoint, and the sentinelctl socket.
type Agent struct {
	cfg         Config
	sched       *ebpf.SchedObserver
	blkio       *ebpf.BlkioObserver
	net         *ebpf.NetObserver
	resolver    *cgroup.Resolver
	timeline    []ebpf.CgroupTimeline      // latest sub-interval ring (issue #4); scored by issue #5 next
	prevThrottle map[uint64]cgroup.CPUStat // last interval's cpu.stat per offender cgroup, for throttle deltas (issue #6)
	baseline    *metrics.Baseline // run-queue latency normals (victim side)
	ioBaseline  *metrics.Baseline // I/O latency normals (victim side)
	netBaseline *metrics.Baseline // retransmit normals (victim side)
	cpuUsage    *metrics.Baseline // CPU-time normals (offender side)
	ioUsage     *metrics.Baseline // disk-throughput normals (offender side)
	netUsage    *metrics.Baseline // network-throughput normals (offender side)
	store       *server.Store
}

// New constructs an Agent with the given config.
func New(cfg Config) *Agent {
	return &Agent{
		cfg:          cfg,
		prevThrottle: map[uint64]cgroup.CPUStat{},
		baseline:    metrics.NewBaseline(cfg.BaselineAlpha, cfg.BaselineWarmup),
		ioBaseline:  metrics.NewBaseline(cfg.BaselineAlpha, cfg.BaselineWarmup),
		netBaseline: metrics.NewBaseline(cfg.BaselineAlpha, cfg.BaselineWarmup),
		cpuUsage:    metrics.NewBaseline(cfg.BaselineAlpha, cfg.BaselineWarmup),
		ioUsage:     metrics.NewBaseline(cfg.BaselineAlpha, cfg.BaselineWarmup),
		netUsage:    metrics.NewBaseline(cfg.BaselineAlpha, cfg.BaselineWarmup),
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

	// Block-I/O observer is best-effort — the agent still runs without it.
	if blk, err := ebpf.LoadBlkio(); err != nil {
		fmt.Printf("warning: blkio observer disabled (%v)\n", err)
	} else {
		a.blkio = blk
		defer a.blkio.Close()
		fmt.Printf("blkio observer attached\n")
	}

	// Network observer is best-effort too.
	if n, err := ebpf.LoadNet(); err != nil {
		fmt.Printf("warning: net observer disabled (%v)\n", err)
	} else {
		a.net = n
		defer a.net.Close()
		fmt.Printf("net observer attached\n")
	}

	// Pod resolution is best-effort — the agent still runs if CRI is down.
	if res, err := cgroup.NewResolver(a.cfg.CRISocket, a.cfg.CgroupRoot); err != nil {
		fmt.Printf("warning: pod resolver disabled (%v); showing raw cgroup IDs\n", err)
	} else {
		a.resolver = res
		defer a.resolver.Close()
		fmt.Printf("pod resolver: %d containers mapped\n", res.Len())

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
	// Drain the sub-interval ring every interval to keep the bounded map clean
	// (read-and-delete). The correlation scorer (issue #5) consumes this next;
	// for now we just stash the latest so the map never fills.
	if tl, err := a.sched.ReadTimeline(); err == nil {
		a.timeline = tl
	}
	var blkio []ebpf.CgroupBlkio
	if a.blkio != nil {
		if io, err := a.blkio.Read(); err == nil {
			blkio = io
		}
	}
	var network []ebpf.CgroupNet
	if a.net != nil {
		if n, err := a.net.Read(); err == nil {
			network = n
		}
	}

	snap := a.buildSnapshot(cpu, runq, blkio, network)
	if a.store != nil {
		a.store.Set(snap)
	}
	a.printSnapshot(snap)
	if a.cfg.ControllerAddr != "" {
		a.reportToController(snap)
	}
	return nil
}

// reportToController POSTs the snapshot to the controller. Best-effort: a
// reachability problem must never disrupt local detection (the agent is
// self-contained and keeps working standalone).
func (a *Agent) reportToController(snap report.Snapshot) {
	body, err := json.Marshal(snap)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.ControllerAddr+"/report", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("controller report failed: %v\n", err)
		return
	}
	_ = resp.Body.Close()
}

// buildSnapshot judges all three dimensions. The node is healthy unless at least
// one pod is genuinely starved of CPU, disk I/O, or network.
func (a *Agent) buildSnapshot(cpu []ebpf.CgroupCPUTime, runq []ebpf.CgroupLatency, blkio []ebpf.CgroupBlkio, network []ebpf.CgroupNet) report.Snapshot {
	snap := report.Snapshot{
		NodeName:         a.cfg.NodeName,
		Time:             time.Now().Format("15:04:05"),
		CgroupsSeen:      len(runq),
		RunqWarnUs:       float64(a.cfg.RunqWarn.Microseconds()),
		MinSamples:       a.cfg.MinSamples,
		ConfidenceMin:    a.cfg.ConfidenceThreshold,
		MaxConfidence:    -1,
		IOMaxConfidence:  -1,
		NetMaxConfidence: -1,
	}

	cpuVictims, cpuWorst := a.cpuVictims(runq)
	ioVictims, ioWorst := a.ioVictims(blkio)
	netVictims, netWorst := a.netVictims(network)

	// Learn each pod's normal resource *usage* every interval — even when
	// healthy — freezing spikes. This is what lets offender attribution find the
	// pod that CHANGED rather than the one that is simply always busy (the
	// "blame the front desk" problem). Offenders below read these baselines.
	a.trackCPUUsage(cpu)
	a.trackIOUsage(blkio)
	a.trackNetUsage(network)

	snap.Healthy = len(cpuVictims) == 0 && len(ioVictims) == 0 && len(netVictims) == 0
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
	if len(netVictims) > 0 {
		snap.NetOffenders = a.netOffenders(network, signalFromRatio(netWorst), &snap.NetMaxConfidence)
		snap.NetVictims = netVictims
	}
	return snap
}

// judgeVictim is the shared victim core for every dimension: a cgroup is a
// victim if its metric is above the absolute floor and, once its baseline is
// warm, at least DeviationFactor times its own normal. It updates the baseline
// (frozen while a known victim) and returns the degradation ratio (0 if cold).
func (a *Agent) judgeVictim(baseline *metrics.Baseline, floor float64, cg uint64, value float64) (bool, float64) {
	ratio, ready := baseline.Deviation(cg, value)

	isVictim := value >= floor
	if ready && ratio < a.cfg.DeviationFactor {
		isVictim = false // warm and not unusual for itself
	}
	baseline.Observe(cg, value, !(ready && isVictim))

	deg := 0.0
	if ready {
		deg = ratio
	}
	return isVictim, deg
}

// cpuVictims: pods starved of CPU (high run-queue latency).
func (a *Agent) cpuVictims(runq []ebpf.CgroupLatency) ([]report.Victim, float64) {
	floor := float64(a.cfg.RunqWarn.Microseconds())
	var out []report.Victim
	worst := 0.0

	for _, r := range runq {
		if r.Count < uint64(a.cfg.MinSamples) {
			continue
		}
		p99 := metrics.Percentile(r.Slots, 99)
		isVictim, deg := a.judgeVictim(a.baseline, floor, r.CgroupID, p99)
		if isVictim {
			if deg > worst {
				worst = deg
			}
			out = append(out, report.Victim{
				Pod: a.label(r.CgroupID), P50us: metrics.Percentile(r.Slots, 50),
				P99us: p99, Degradation: deg, Events: r.Count,
			})
		}
	}
	a.baseline.Prune()
	sort.Slice(out, func(i, j int) bool { return out[i].P99us > out[j].P99us })
	return capVictims(out, a.cfg.TopN), worst
}

// ioVictims: pods waiting on the disk (high I/O latency).
func (a *Agent) ioVictims(blkio []ebpf.CgroupBlkio) ([]report.Victim, float64) {
	floor := float64(a.cfg.IOWarn.Microseconds())
	var out []report.Victim
	worst := 0.0

	for _, r := range blkio {
		if r.Count < uint64(a.cfg.MinOps) {
			continue
		}
		p99 := metrics.Percentile(r.Slots, 99)
		isVictim, deg := a.judgeVictim(a.ioBaseline, floor, r.CgroupID, p99)
		if isVictim {
			if deg > worst {
				worst = deg
			}
			out = append(out, report.Victim{
				Pod: a.label(r.CgroupID), P50us: metrics.Percentile(r.Slots, 50),
				P99us: p99, Degradation: deg, Events: r.Count,
			})
		}
	}
	a.ioBaseline.Prune()
	sort.Slice(out, func(i, j int) bool { return out[i].P99us > out[j].P99us })
	return capVictims(out, a.cfg.TopN), worst
}

// netVictims: pods whose TCP segments are being retransmitted.
func (a *Agent) netVictims(network []ebpf.CgroupNet) ([]report.NetVictim, float64) {
	floor := float64(a.cfg.RetransWarn)
	var out []report.NetVictim
	worst := 0.0

	for _, r := range network {
		if r.TxSegs < uint64(a.cfg.MinSegs) {
			continue
		}
		isVictim, deg := a.judgeVictim(a.netBaseline, floor, r.CgroupID, float64(r.Retransmits))
		if isVictim {
			if deg > worst {
				worst = deg
			}
			rate := 0.0
			if r.TxSegs > 0 {
				rate = float64(r.Retransmits) / float64(r.TxSegs) * 100
			}
			out = append(out, report.NetVictim{
				Pod: a.label(r.CgroupID), Retransmits: r.Retransmits,
				RatePct: rate, Degradation: deg, Segs: r.TxSegs,
			})
		}
	}
	a.netBaseline.Prune()
	sort.Slice(out, func(i, j int) bool { return out[i].Retransmits > out[j].Retransmits })
	if len(out) > a.cfg.TopN {
		out = out[:a.cfg.TopN]
	}
	return out, worst
}

func capVictims(v []report.Victim, n int) []report.Victim {
	if len(v) > n {
		return v[:n]
	}
	return v
}

// signalFromRatio maps a worst-victim degradation ratio to a 0-1 severity used
// to cap offender confidence. With no warm victim we only have the absolute
// signal, so use a moderate default rather than overclaiming.
func signalFromRatio(worst float64) float64 {
	if worst > 1 {
		return clamp((worst-1)/4, 0, 1)
	}
	return 0.5
}

// cpuOffenders ranks cgroups by CPU time and scores confidence vs fair share.
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
	newThrottle := make(map[uint64]cgroup.CPUStat, len(cpu))
	for _, r := range cpu {
		intensity := 0.0
		if totalNs > 0 {
			intensity = float64(r.OnCpuNs) / float64(totalNs) * 100
		}
		o := report.Offender{
			Pod: a.label(r.CgroupID), CPUms: float64(r.OnCpuNs) / 1e6,
			Intensity: intensity, ReqMilli: -1, Confidence: -1,
			Verdict: "system / unattributed",
		}
		if pod, ok := a.resolve(r.CgroupID); ok {
			o.ReqMilli = pod.RequestMilliCPU
			o.Verdict = fairShareVerdict(intensity, pod.RequestMilliCPU, totalReq)
			// CFS throttle pressure this interval (issue #6): fraction of periods
			// the pod hit its quota, from the cpu.stat delta vs last interval.
			// First sight (no prev) seeds the baseline and reads as 0.
			throttle := a.throttleFraction(r.CgroupID, newThrottle)
			o.ThrottlePct = throttle * 100
			// Warmup fallback (before the usage baseline is warm): excess over
			// the pod's CPU request — available instantly from the request, no
			// learning needed. Once warm, deviation-from-own-normal takes over.
			fallback := -1.0
			if totalReq > 0 {
				fair := float64(pod.RequestMilliCPU) / float64(totalReq) * 100
				fallback = clamp((intensity-fair)/100/0.5, 0, 1)
			}
			o.Confidence = a.offenderConfidence(a.cpuUsage, r.CgroupID, float64(r.OnCpuNs), intensity, victimSignal, fallback, throttle)
			if o.Confidence > *maxConf {
				*maxConf = o.Confidence
			}
		}
		out = append(out, o)
	}
	// Keep only cgroups read this interval as next round's baseline — bounds the
	// map to the offender set and drops departed cgroups.
	a.prevThrottle = newThrottle
	return out
}

// throttleFraction reads a cgroup's cpu.stat, records it in cur for next round,
// and returns the fraction of CFS periods throttled since last interval (0 on
// first sight, on a counter reset, or when the cgroup has no CPU quota).
func (a *Agent) throttleFraction(cgroupID uint64, cur map[uint64]cgroup.CPUStat) float64 {
	if a.resolver == nil {
		return 0
	}
	now, ok := a.resolver.ReadCPUStat(cgroupID)
	if !ok {
		return 0
	}
	cur[cgroupID] = now
	prev, had := a.prevThrottle[cgroupID]
	if !had {
		return 0
	}
	return now.Sub(prev).ThrottledFraction()
}

// ioOffenders ranks cgroups by disk throughput; confidence from share of bytes.
func (a *Agent) ioOffenders(blkio []ebpf.CgroupBlkio, victimSignal float64, maxConf *float64) []report.IOOffender {
	var total uint64
	for _, r := range blkio {
		total += r.Bytes
	}
	sort.Slice(blkio, func(i, j int) bool { return blkio[i].Bytes > blkio[j].Bytes })
	if len(blkio) > a.cfg.TopN {
		blkio = blkio[:a.cfg.TopN]
	}

	out := make([]report.IOOffender, 0, len(blkio))
	for _, r := range blkio {
		o := report.IOOffender{
			Pod: a.label(r.CgroupID), MB: float64(r.Bytes) / 1e6,
			SharePct: share(r.Bytes, total), Ops: r.Count, Confidence: -1,
		}
		if _, ok := a.resolve(r.CgroupID); ok {
			o.Confidence = a.offenderConfidence(a.ioUsage, r.CgroupID, float64(r.Bytes), o.SharePct, victimSignal, -1, 0)
			if o.Confidence > *maxConf {
				*maxConf = o.Confidence
			}
		}
		out = append(out, o)
	}
	return out
}

// netOffenders ranks cgroups by TX throughput; confidence from share of bytes.
func (a *Agent) netOffenders(network []ebpf.CgroupNet, victimSignal float64, maxConf *float64) []report.NetOffender {
	var total uint64
	for _, r := range network {
		total += r.TxBytes
	}
	sort.Slice(network, func(i, j int) bool { return network[i].TxBytes > network[j].TxBytes })
	if len(network) > a.cfg.TopN {
		network = network[:a.cfg.TopN]
	}

	out := make([]report.NetOffender, 0, len(network))
	for _, r := range network {
		o := report.NetOffender{
			Pod: a.label(r.CgroupID), MB: float64(r.TxBytes) / 1e6,
			SharePct: share(r.TxBytes, total), Segs: r.TxSegs, Confidence: -1,
		}
		if _, ok := a.resolve(r.CgroupID); ok {
			o.Confidence = a.offenderConfidence(a.netUsage, r.CgroupID, float64(r.TxBytes), o.SharePct, victimSignal, -1, 0)
			if o.Confidence > *maxConf {
				*maxConf = o.Confidence
			}
		}
		out = append(out, o)
	}
	return out
}

func share(part, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

// trackCPUUsage/trackIOUsage/trackNetUsage learn each cgroup's normal resource
// usage. Run every interval, including healthy ones, so the baseline knows each
// pod's normal — and freeze a pod while it is spiking so the spike stays visible.
func (a *Agent) trackCPUUsage(cpu []ebpf.CgroupCPUTime) {
	for _, r := range cpu {
		a.observeUsage(a.cpuUsage, r.CgroupID, float64(r.OnCpuNs))
	}
	a.cpuUsage.Prune()
}

func (a *Agent) trackIOUsage(blkio []ebpf.CgroupBlkio) {
	for _, r := range blkio {
		a.observeUsage(a.ioUsage, r.CgroupID, float64(r.Bytes))
	}
	a.ioUsage.Prune()
}

func (a *Agent) trackNetUsage(network []ebpf.CgroupNet) {
	for _, r := range network {
		a.observeUsage(a.netUsage, r.CgroupID, float64(r.TxBytes))
	}
	a.netUsage.Prune()
}

func (a *Agent) observeUsage(b *metrics.Baseline, cg uint64, usage float64) {
	ratio, ready := b.Deviation(cg, usage)
	spiking := ready && ratio >= a.cfg.DeviationFactor
	b.Observe(cg, usage, !spiking)
}

// offenderConfidence scores a pod's likelihood of being the noisy neighbour as
// the minimum of three signals — all must hold:
//   - it CHANGED: usage is far above its own learned normal (the pod that spiked,
//     not the one that is simply always busy). Before the baseline is warm this
//     uses the dimension's fallback (fair share for CPU; none for disk/net).
//   - it's BIG ENOUGH: it holds a meaningful share of the resource, so a tiny
//     pod jumping from near-zero to near-zero can't read as the culprit.
//   - there is real HARM: victims are actually degraded (victimSignal).
//
// throttle (issue #6, CPU only; 0 elsewhere) is CFS-throttle pressure in [0,1] —
// the fraction of periods the pod hit its quota this interval. Throttling is
// direct evidence a pod is backing up the scheduler, so it stands in for the
// CHANGED signal (via max): it corroborates a spike, and works even before the
// learned baseline is warm. Magnitude and harm still gate via min.
func (a *Agent) offenderConfidence(b *metrics.Baseline, cg uint64, usage, sharePct, victimSignal, fallback, throttle float64) float64 {
	magnitude := clamp(sharePct/100/0.25, 0, 1) // 25% of the resource → full

	changed := fallback
	if ratio, ready := b.Deviation(cg, usage); ready {
		changed = clamp((ratio-1)/4, 0, 1) // 5x its own normal → full
	}
	if throttle > 0 {
		changed = max(changed, clamp(throttle, 0, 1)) // throttling stands in for deviation
	}
	if changed < 0 {
		return -1 // cold baseline, no fallback, no throttle — honestly cannot attribute yet
	}

	return clamp(min(min(changed, magnitude), victimSignal), 0, 1)
}

// fairShareVerdict compares a cgroup's CPU intensity to the share implied by its
// CPU request.
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
		fmt.Printf("%s  [OK] healthy — no contention (CPU + disk + network nominal; %d cgroups seen)\n", s.Time, s.CgroupsSeen)
		return
	}

	fmt.Printf("\n%s  [!] CONTENTION — CPU: %d, I/O: %d, NET: %d victim(s)\n",
		s.Time, len(s.Victims), len(s.IOVictims), len(s.NetVictims))

	if len(s.Victims) > 0 {
		fmt.Printf("  ── CPU ──  %s\n", attribution(s.MaxConfidence, s.ConfidenceMin))
		fmt.Printf("  OFFENDERS — by CPU time\n")
		fmt.Printf("  %-42s %9s %9s %7s %8s %10s  %s\n", "POD", "CPU_MS", "INTENSITY", "REQ_m", "THROTTLE", "CONFIDENCE", "VERDICT")
		for _, o := range s.Offenders {
			fmt.Printf("  %-42s %9.0f %8.1f%% %7s %8s %10s  %s\n",
				truncate(o.Pod, 42), o.CPUms, o.Intensity, reqStr(o.ReqMilli), throtStr(o.ThrottlePct), confStr(o.Confidence), o.Verdict)
		}
		a.printLatencyVictims("run-queue latency", s.Victims)
	}

	if len(s.IOVictims) > 0 {
		fmt.Printf("  ── DISK I/O ──  %s\n", attribution(s.IOMaxConfidence, s.ConfidenceMin))
		fmt.Printf("  OFFENDERS — by disk throughput\n")
		fmt.Printf("  %-42s %10s %9s %8s %10s\n", "POD", "MB", "SHARE", "OPS", "CONFIDENCE")
		for _, o := range s.IOOffenders {
			fmt.Printf("  %-42s %10.1f %8.1f%% %8d %10s\n",
				truncate(o.Pod, 42), o.MB, o.SharePct, o.Ops, confStr(o.Confidence))
		}
		a.printLatencyVictims("I/O latency", s.IOVictims)
	}

	if len(s.NetVictims) > 0 {
		fmt.Printf("  ── NETWORK ──  %s\n", attribution(s.NetMaxConfidence, s.ConfidenceMin))
		fmt.Printf("  OFFENDERS — by TX throughput\n")
		fmt.Printf("  %-42s %10s %9s %8s %10s\n", "POD", "TX_MB", "SHARE", "SEGS", "CONFIDENCE")
		for _, o := range s.NetOffenders {
			fmt.Printf("  %-42s %10.1f %8.1f%% %8d %10s\n",
				truncate(o.Pod, 42), o.MB, o.SharePct, o.Segs, confStr(o.Confidence))
		}
		fmt.Printf("  VICTIMS — by TCP retransmits\n")
		fmt.Printf("  %-42s %12s %10s %9s %8s\n", "POD", "RETRANSMITS", "RATE", "xBASELINE", "SEGS")
		for _, v := range s.NetVictims {
			fmt.Printf("  %-42s %12d %9.1f%% %9s %8d\n",
				truncate(v.Pod, 42), v.Retransmits, v.RatePct, degStr(v.Degradation), v.Segs)
		}
	}
}

func (a *Agent) printLatencyVictims(metric string, rows []report.Victim) {
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
		return "attribution: no confident pod offender (still learning baselines, or a system process) — alert only"
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

func throtStr(p float64) string {
	if p <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", p)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
