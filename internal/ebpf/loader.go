//go:build linux

// Package ebpf loads the compiled BPF objects, attaches their programs, and
// reads their maps. It builds on Linux only. Per design §7.2.1 the package is
// split into loader.go (load + attach), one file per observer (sched.go, and
// later blkio.go/net.go/...), types.go (Go map representations), and ringbuf.go.
package ebpf

import (
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

// bpf2go compiles sched_monitor.bpf.c and generates Go bindings (sched_bp*.go
// plus the embedded .o). Run via `make generate` on the Linux build host — it
// needs clang and a vmlinux.h dumped from the host's BTF (`make vmlinux`).
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -target bpfel -type sched_hist -type sched_bucket -type sched_buckets sched bpf/sched_monitor.bpf.c -- -I./bpf -O2 -g -Wall
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -target bpfel -type blkio_hist blkio bpf/blkio_monitor.bpf.c -- -I./bpf -O2 -g -Wall
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -target bpfel -type net_stats net bpf/net_monitor.bpf.c -- -I./bpf -O2 -g -Wall
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -target bpfel -type cgroup_event cgroup bpf/cgroup_monitor.bpf.c -- -I./bpf -O2 -g -Wall

// SchedObserver loads sched_monitor, attaches its tracepoints, and exposes the
// per-cgroup run-queue latency map for reading.
type SchedObserver struct {
	objs  schedObjects
	links []link.Link
}

// LoadSched loads the embedded BPF objects and attaches the scheduler
// tracepoints. Call Close to detach and release resources.
func LoadSched() (*SchedObserver, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("remove memlock rlimit: %w", err)
	}

	o := &SchedObserver{}
	if err := loadSchedObjects(&o.objs, nil); err != nil {
		return nil, fmt.Errorf("load bpf objects: %w", err)
	}

	for name, prog := range map[string]*ebpf.Program{
		"sched_wakeup":     o.objs.HandleSchedWakeup,
		"sched_wakeup_new": o.objs.HandleSchedWakeupNew,
		"sched_switch":     o.objs.HandleSchedSwitch,
	} {
		l, err := link.AttachTracing(link.TracingOptions{Program: prog})
		if err != nil {
			o.Close()
			return nil, fmt.Errorf("attach %s: %w", name, err)
		}
		o.links = append(o.links, l)
	}

	return o, nil
}

// Close detaches all programs and frees BPF resources.
func (o *SchedObserver) Close() {
	for _, l := range o.links {
		_ = l.Close()
	}
	o.objs.Close()
}
