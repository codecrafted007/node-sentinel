//go:build linux

package ebpf

import (
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
)

// BlkioObserver loads blkio_monitor, attaches its block-layer tracepoints, and
// exposes the per-cgroup I/O latency + throughput map.
type BlkioObserver struct {
	objs  blkioObjects
	links []link.Link
}

// LoadBlkio loads the embedded BPF objects and attaches the block tracepoints.
func LoadBlkio() (*BlkioObserver, error) {
	o := &BlkioObserver{}
	if err := loadBlkioObjects(&o.objs, nil); err != nil {
		return nil, fmt.Errorf("load blkio objects: %w", err)
	}

	for name, prog := range map[string]*ebpf.Program{
		"block_rq_insert":   o.objs.HandleBlockRqInsert,
		"block_rq_complete": o.objs.HandleBlockRqComplete,
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

// Read snapshots the per-cgroup I/O map: sum the per-CPU copies, then delete
// each key so the next interval starts clean (read-and-delete; design §7.2.3).
func (o *BlkioObserver) Read() ([]CgroupBlkio, error) {
	m := o.objs.BlkioLatencyMap

	var (
		key     uint64
		percpu  []blkioBlkioHist
		results []CgroupBlkio
		keys    []uint64
	)

	it := m.Iterate()
	for it.Next(&key, &percpu) {
		if len(percpu) == 0 {
			continue
		}
		agg := CgroupBlkio{CgroupID: key, Slots: make([]uint64, len(percpu[0].Slots))}
		for _, cpu := range percpu {
			for i, v := range cpu.Slots {
				agg.Slots[i] += v
			}
			agg.TotalUs += cpu.TotalUs
			agg.Count += cpu.Count
			agg.Bytes += cpu.Bytes
		}
		results = append(results, agg)
		keys = append(keys, key)
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("iterate blkio_latency_map: %w", err)
	}

	for i := range keys {
		_ = m.Delete(&keys[i])
	}
	return results, nil
}

// Close detaches all programs and frees BPF resources.
func (o *BlkioObserver) Close() {
	for _, l := range o.links {
		_ = l.Close()
	}
	o.objs.Close()
}
