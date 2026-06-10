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
}

// DefaultConfig returns the Phase 1 defaults (design §7.2.2 / §7.4).
func DefaultConfig() Config {
	return Config{
		ReadInterval:   5 * time.Second,
		TopN:           20,
		CRISocket:      "unix:///run/containerd/containerd.sock",
		CgroupRoot:     "/sys/fs/cgroup/kubepods.slice",
		ResolveRefresh: 30 * time.Second,
	}
}
