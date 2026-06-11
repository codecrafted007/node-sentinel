// Package agent is the top-level node-agent lifecycle (start, stop, reload) per
// design §7.2.1. Phase 1 is intentionally small: load the scheduler observer,
// resolve cgroups to pods, and print live per-pod run-queue latency.
package agent

import "time"

// Config holds agent tuning. It grows to observer toggles, controller endpoint,
// etc. in later phases (design §7.2.1 config.go).
type Config struct {
	// ReadInterval is how often the agent snapshots the BPF maps.
	ReadInterval time.Duration
	// TopN limits how many cgroups are printed each interval.
	TopN int
	// CRISocket is the containerd/CRI-O CRI endpoint for pod resolution.
	CRISocket string
	// CgroupRoot is the cgroups v2 subtree to scan for pod containers.
	CgroupRoot string
	// ResolveRefresh is how often the cgroup->pod map is rebuilt (design §7.4
	// keeps a 60s full rescan as a safety net; we drive it on this interval).
	ResolveRefresh time.Duration

	// --- contention thresholds (the first real policy knobs) ---

	// MinSamples is how many run-queue measurements a cgroup needs in an
	// interval before its p99 is trusted. Below this the percentile is noise
	// (a p99 over 30 samples is just "the worst one"), so we ignore it.
	MinSamples int
	// RunqWarn is the run-queue p99 a pod must exceed to count as a victim of
	// contention. Below it, the wait is normal time-sharing, not a problem.
	// This absolute floor is the primary, restart-safe signal; the baseline
	// below only refines it once warm.
	RunqWarn time.Duration

	// --- adaptive baseline + confidence (design §7.5) ---

	// DeviationFactor: once a pod's baseline is warm, its current run-queue p99
	// must be at least this many times its own normal to count as a victim
	// (so a pod that is *always* a bit slow isn't flagged for being itself).
	DeviationFactor float64
	// BaselineAlpha is the EMA smoothing for the learned normal (0-1; higher
	// reacts faster). BaselineWarmup is how many intervals before it's trusted.
	BaselineAlpha  float64
	BaselineWarmup int
	// ConfidenceThreshold is the offender confidence needed before we'd call a
	// pod the noisy neighbour (and, in future, act on it). Below it we alert only.
	ConfidenceThreshold float64

	// --- disk I/O dimension ---

	// IOWarn is the I/O latency p99 a pod must exceed to count as an I/O victim.
	IOWarn time.Duration
	// MinOps is how many completed I/O requests a cgroup needs before its I/O
	// p99 is trusted (the I/O analogue of MinSamples).
	MinOps int

	// --- network dimension ---

	// RetransWarn is the TCP retransmit count in an interval a pod must exceed
	// to count as a network victim.
	RetransWarn int
	// MinSegs is how many sendmsg calls a cgroup needs before its retransmits
	// are judged (enough network activity to be meaningful).
	MinSegs int

	// --- observability surfaces ---

	// MetricsAddr is the Prometheus /metrics listen address ("" disables it).
	MetricsAddr string
	// LocalSocket is the unix socket sentinelctl connects to ("" disables it).
	LocalSocket string

	// --- controller reporting ---

	// ControllerAddr is the controller's base URL (e.g. http://host:8080).
	// Empty means standalone — the agent just reports locally.
	ControllerAddr string
	// NodeName is how this node identifies itself to the controller.
	NodeName string
}

// DefaultConfig returns the Phase 1 defaults (design §7.2.2 / §7.4).
func DefaultConfig() Config {
	return Config{
		ReadInterval:   5 * time.Second,
		TopN:           20,
		CRISocket:      "unix:///run/containerd/containerd.sock",
		CgroupRoot:     "/sys/fs/cgroup/kubepods.slice",
		ResolveRefresh: 60 * time.Second, // safety-net rescan; the watcher handles liveness
		MinSamples:          100,
		RunqWarn:            5 * time.Millisecond,
		DeviationFactor:     3.0,
		BaselineAlpha:       0.15,
		BaselineWarmup:      3,
		ConfidenceThreshold: 0.7,
		IOWarn:              20 * time.Millisecond,
		MinOps:              20,
		RetransWarn:         10,
		MinSegs:             50,
		MetricsAddr:         ":2112",
		LocalSocket:         "/var/run/sentinel/agent.sock",
	}
}
