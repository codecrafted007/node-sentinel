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

// PodID identifies the pod/container a cgroup belongs to.
type PodID struct {
	Namespace string
	Pod       string
	Container string
	PodUID    string
}

func (p PodID) String() string {
	if p.Namespace == "" {
		return "unknown"
	}
	return p.Namespace + "/" + p.Pod + "/" + p.Container
}

// Resolver maps cgroup_id -> PodID, rebuilt by Refresh from the cgroup tree + CRI.
type Resolver struct {
	cgroupRoot string
	conn       *grpc.ClientConn
	rt         runtimeapi.RuntimeServiceClient

	mu    sync.RWMutex
	cache map[uint64]PodID
}

// NewResolver dials the CRI socket and performs an initial scan.
func NewResolver(criSocket, cgroupRoot string) (*Resolver, error) {
	conn, err := grpc.NewClient(criSocket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial CRI %s: %w", criSocket, err)
	}
	r := &Resolver{
		cgroupRoot: cgroupRoot,
		conn:       conn,
		rt:         runtimeapi.NewRuntimeServiceClient(conn),
		cache:      map[uint64]PodID{},
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.cache[cgroupID]
	return p, ok
}

// Len reports how many cgroups are currently resolved (diagnostics).
func (r *Resolver) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.cache)
}

// Refresh rebuilds the cgroup_id -> PodID map: scan the cgroup tree for
// container scopes (inode = cgroup_id, dir name carries the container ID), then
// join against CRI container metadata.
func (r *Resolver) Refresh(ctx context.Context) error {
	cidToCgroupID, err := r.scanCgroups()
	if err != nil {
		return fmt.Errorf("scan cgroups: %w", err)
	}

	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := r.rt.ListContainers(cctx, &runtimeapi.ListContainersRequest{})
	if err != nil {
		return fmt.Errorf("CRI ListContainers: %w", err)
	}

	next := make(map[uint64]PodID, len(cidToCgroupID))
	for _, c := range resp.GetContainers() {
		cgid, ok := cidToCgroupID[c.GetId()]
		if !ok {
			continue
		}
		l := c.GetLabels()
		next[cgid] = PodID{
			Namespace: l["io.kubernetes.pod.namespace"],
			Pod:       l["io.kubernetes.pod.name"],
			Container: l["io.kubernetes.container.name"],
			PodUID:    l["io.kubernetes.pod.uid"],
		}
	}

	r.mu.Lock()
	r.cache = next
	r.mu.Unlock()
	return nil
}

// Close releases the CRI connection.
func (r *Resolver) Close() error { return r.conn.Close() }

// scanCgroups walks the cgroup tree and returns containerID -> cgroup_id(inode).
// On cgroups v2 with 64-bit inodes (kernel 5.5+), the kernfs node id eBPF reads
// equals the directory inode reported by stat, so this join is exact.
func (r *Resolver) scanCgroups() (map[string]uint64, error) {
	out := map[string]uint64{}
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
		out[m[1]] = st.Ino
		return nil
	})
	return out, err
}
