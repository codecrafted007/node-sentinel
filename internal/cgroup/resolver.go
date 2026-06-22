//go:build linux

// Package cgroup resolves kernel cgroup IDs (the inode numbers eBPF sees) to
// Kubernetes pod identities, per design §7.4. It scans the cgroups v2 tree to
// map cgroup_id -> container ID, then queries the CRI runtime to map container
// ID -> namespace/pod/container.
//
// The inotify-based live watcher (design §7.4 "ongoing updates", watcher.go) is
// deferred; for now Refresh is driven on an interval by the agent, which is the
// design's 60s full-rescan safety net.
package cgroup

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// Defaults for containerd with the systemd cgroup driver.
const (
	DefaultCgroupRoot = "/sys/fs/cgroup/kubepods.slice"
	DefaultCRISocket  = "unix:///run/containerd/containerd.sock"
)

// Leaf container-scope directory names across runtimes (design §7.4):
//   containerd: cri-containerd-<64hex>.scope
//   CRI-O:      crio-<64hex>.scope
//   docker:     docker-<64hex>.scope
var scopeRe = regexp.MustCompile(`(?:cri-containerd-|crio-|docker-)([0-9a-f]{64})\.scope$`)

// Resolver maps cgroup_id -> PodID, rebuilt by Refresh from the cgroup tree + CRI.
type Resolver struct {
	cgroupRoot string
	conn       *grpc.ClientConn
	rt         runtimeapi.RuntimeServiceClient

	refreshMu sync.Mutex // serializes Refresh (watcher + periodic rescan)

	cache *ttlCache // cgroup_id -> PodID, with a grace period so vanished cgroups stay nameable (issue #3)

	mu    sync.RWMutex
	paths map[uint64]string // cgroup_id -> cgroup dir, for per-interval cpu.stat reads
}

// cgroupScope is a container's cgroup directory found during a scan: its inode
// (which equals the cgroup_id eBPF reports) and the directory path, kept so
// per-interval reads like cpu.stat (issue #6) can find the file.
type cgroupScope struct {
	ino uint64
	dir string
}

// NewResolver dials the CRI socket and performs an initial scan. cacheTTL is how
// long a vanished cgroup's name is retained so late stats still resolve (issue #3).
func NewResolver(criSocket, cgroupRoot string, cacheTTL time.Duration) (*Resolver, error) {
	conn, err := grpc.NewClient(criSocket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial CRI %s: %w", criSocket, err)
	}
	r := &Resolver{
		cgroupRoot: cgroupRoot,
		conn:       conn,
		rt:         runtimeapi.NewRuntimeServiceClient(conn),
		cache:      newTTLCache(cacheTTL),
		paths:      map[uint64]string{},
	}
	if err := r.Refresh(context.Background()); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return r, nil
}

// Resolve returns the pod identity for a cgroup_id; ok is false for cgroups not
// backed by a CRI container (system slices, pause sandboxes) — never attributed.
func (r *Resolver) Resolve(cgroupID uint64) (PodID, bool) {
	return r.cache.get(cgroupID)
}

// Len reports how many cgroups are currently resolved, including those within
// their post-teardown grace period (diagnostics).
func (r *Resolver) Len() int {
	return r.cache.len()
}

// Refresh rebuilds the cgroup_id -> PodID map: scan the cgroup tree for
// container scopes (inode = cgroup_id, dir name carries the container ID), then
// join against CRI container metadata.
func (r *Resolver) Refresh(ctx context.Context) error {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()

	cidToScope, err := r.scanCgroups()
	if err != nil {
		return fmt.Errorf("scan cgroups: %w", err)
	}

	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := r.rt.ListContainers(cctx, &runtimeapi.ListContainersRequest{})
	if err != nil {
		return fmt.Errorf("CRI ListContainers: %w", err)
	}

	next := make(map[uint64]PodID, len(cidToScope))
	nextPaths := make(map[uint64]string, len(cidToScope))
	for _, c := range resp.GetContainers() {
		sc, ok := cidToScope[c.GetId()]
		if !ok {
			continue
		}
		l := c.GetLabels()
		next[sc.ino] = PodID{
			Namespace:       l["io.kubernetes.pod.namespace"],
			Pod:             l["io.kubernetes.pod.name"],
			Container:       l["io.kubernetes.container.name"],
			PodUID:          l["io.kubernetes.pod.uid"],
			RequestMilliCPU: r.requestMilliCPU(ctx, c.GetId()),
		}
		nextPaths[sc.ino] = sc.dir
	}

	r.cache.replace(next) // merge: live entries refresh, vanished ones keep their name until TTL
	r.mu.Lock()
	r.paths = nextPaths
	r.mu.Unlock()
	return nil
}

// Tombstone marks a cgroup as torn down, starting its name's grace period (issue
// #2: driven by a cgroup_rmdir event). The final-interval stats can still resolve
// until the TTL elapses. Exported for the lifecycle watcher.
func (r *Resolver) Tombstone(cgroupID uint64) { r.cache.tombstone(cgroupID) }

// ResolveCgroupPath does a single-container CRI join for a freshly-created cgroup
// and records the binding (issue #2: the lifecycle watcher's lazy join on
// cgroup_mkdir, so a container that dies before the next full rescan is still
// named). path is the cgroup path from the kernel event; it returns false (and
// records nothing) for cgroups that aren't a CRI container scope, or when CRI
// doesn't know the container yet (the periodic rescan is the backstop).
func (r *Resolver) ResolveCgroupPath(ctx context.Context, cgroupID uint64, path string) bool {
	m := scopeRe.FindStringSubmatch(path)
	if m == nil {
		return false // a slice / sandbox, not a container scope
	}

	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	resp, err := r.rt.ContainerStatus(cctx, &runtimeapi.ContainerStatusRequest{ContainerId: m[1]})
	if err != nil {
		return false // CRI hasn't registered it yet — rescan will catch it
	}

	l := resp.GetStatus().GetLabels()
	if l["io.kubernetes.pod.namespace"] == "" {
		return false
	}
	req := int64(0)
	if shares := resp.GetStatus().GetResources().GetLinux().GetCpuShares(); shares >= 2 {
		req = shares * 1000 / 1024
	}
	r.cache.put(cgroupID, PodID{
		Namespace:       l["io.kubernetes.pod.namespace"],
		Pod:             l["io.kubernetes.pod.name"],
		Container:       l["io.kubernetes.container.name"],
		PodUID:          l["io.kubernetes.pod.uid"],
		RequestMilliCPU: req,
	})
	return true
}

// ReadCPUStat reads and parses cpu.stat for a cgroup (issue #6). ok is false when
// the cgroup isn't resolved or the file can't be read; a cgroup with no CPU quota
// still reads fine (all-zero throttling), so ok=false means truly unavailable.
func (r *Resolver) ReadCPUStat(cgroupID uint64) (CPUStat, bool) {
	r.mu.RLock()
	dir, ok := r.paths[cgroupID]
	r.mu.RUnlock()
	if !ok {
		return CPUStat{}, false
	}

	f, err := os.Open(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return CPUStat{}, false
	}
	defer f.Close()

	s, err := parseCPUStat(f)
	if err != nil {
		return CPUStat{}, false
	}
	return s, true
}

// requestMilliCPU returns a container's CPU request in millicores, derived from
// the CPU shares the runtime reports for it. Kubernetes maps a CPU request to
// shares as shares = milli * 1024 / 1000, so we invert that. Returns 0 when the
// runtime reports nothing (best-effort pods, or an older CRI).
func (r *Resolver) requestMilliCPU(ctx context.Context, id string) int64 {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	resp, err := r.rt.ContainerStatus(cctx, &runtimeapi.ContainerStatusRequest{ContainerId: id})
	if err != nil {
		return 0
	}
	shares := resp.GetStatus().GetResources().GetLinux().GetCpuShares()
	if shares < 2 {
		return 0
	}
	return shares * 1000 / 1024
}

// Close releases the CRI connection.
func (r *Resolver) Close() error { return r.conn.Close() }

// scanCgroups walks the cgroup tree and returns containerID -> cgroup scope
// (inode + dir). On cgroups v2 with 64-bit inodes (kernel 5.5+), the kernfs node
// id eBPF reads equals the directory inode reported by stat, so this join is
// exact; the dir is kept for per-interval cpu.stat reads.
func (r *Resolver) scanCgroups() (map[string]cgroupScope, error) {
	out := map[string]cgroupScope{}
	err := filepath.WalkDir(r.cgroupRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries (races with pod deletion)
		}
		if !d.IsDir() {
			return nil
		}
		m := scopeRe.FindStringSubmatch(d.Name())
		if m == nil {
			return nil
		}
		var st syscall.Stat_t
		if err := syscall.Stat(path, &st); err != nil {
			return nil
		}
		out[m[1]] = cgroupScope{ino: st.Ino, dir: path}
		return nil
	})
	return out, err
}
