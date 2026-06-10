//go:build linux

package ebpf

import "fmt"

// Read snapshots the run-queue latency map: for each cgroup it sums the per-CPU
// histogram copies, then deletes the entry so the next interval starts clean
// (read-and-delete; design §7.2.3).
func (o *SchedObserver) Read() ([]CgroupLatency, error) {
	m := o.objs.RunqLatencyMap

	var (
		key     uint64
		percpu  []schedSchedHist
		results []CgroupLatency
		keys    []uint64
	)

	it := m.Iterate()
	for it.Next(&key, &percpu) {
		if len(percpu) == 0 {
			continue
		}
		agg := CgroupLatency{
			CgroupID: key,
			Slots:    make([]uint64, len(percpu[0].Slots)),
		}
		for _, cpu := range percpu {
			for i, v := range cpu.Slots {
				agg.Slots[i] += v
			}
			agg.TotalUs += cpu.TotalUs
			agg.Count += cpu.Count
		}
		results = append(results, agg)
		keys = append(keys, key)
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("iterate runq_latency_map: %w", err)
	}

	for i := range keys {
		_ = m.Delete(&keys[i])
	}
	return results, nil
}

// ReadCPU snapshots the per-cgroup on-CPU time map: sum the per-CPU counters,
// then delete each key so the next interval starts clean (same read-and-delete
// pattern as Read).
func (o *SchedObserver) ReadCPU() ([]CgroupCPUTime, error) {
	m := o.objs.CpuTimeMap

	var (
		key     uint64
		percpu  []uint64
		results []CgroupCPUTime
		keys    []uint64
	)

	it := m.Iterate()
	for it.Next(&key, &percpu) {
		var sum uint64
		for _, v := range percpu {
			sum += v
		}
		results = append(results, CgroupCPUTime{CgroupID: key, OnCpuNs: sum})
		keys = append(keys, key)
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("iterate cputime_map: %w", err)
	}

	for i := range keys {
		_ = m.Delete(&keys[i])
	}
	return results, nil
}
