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
	RunqWarn time.Duration

	// --- observability surfaces ---

	// MetricsAddr is the Prometheus /metrics listen address ("" disables it).
	MetricsAddr string
	// LocalSocket is the unix socket sentinelctl connects to ("" disables it).
	LocalSocket string
}

// DefaultConfig returns the Phase 1 defaults (design §7.2.2 / §7.4).
func DefaultConfig() Config {
	return Config{
		ReadInterval:   5 * time.Second,
		TopN:           20,
		CRISocket:      "unix:///run/containerd/containerd.sock",
		CgroupRoot:     "/sys/fs/cgroup/kubepods.slice",
		ResolveRefresh: 30 * time.Second,
		MinSamples:     100,
		RunqWarn:       5 * time.Millisecond,
		MetricsAddr:    ":2112",
		LocalSocket:    "/var/run/sentinel/agent.sock",
	}
}
