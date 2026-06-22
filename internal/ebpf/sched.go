//go:build linux

package ebpf

import (
	"fmt"

	"golang.org/x/sys/unix"
)

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

// ReadTimeline drains the sub-interval ring (issue #4) and re-aligns it: for
// each cgroup it folds the per-CPU copies onto one shared epoch axis ending at
// "now", so every returned series spans the SAME windows and an offender's CPU
// series lines up bucket-for-bucket with a victim's run-queue series (they are
// different cgroups, so a common axis is essential for correlation).
//
// The per-CPU copies each carry their own epoch per slot — a slot can hold
// different windows on different CPUs — so we can't sum slot i blindly; we bin
// every slot by its absolute epoch and drop anything older than the ring or
// ahead of now (a stale revolution). Read-and-delete like the other maps.
func (o *SchedObserver) ReadTimeline() ([]CgroupTimeline, error) {
	m := o.objs.SchedTimelineMap

	// "now" on the same clock the kernel stamps with (bpf_ktime_get_ns is
	// CLOCK_MONOTONIC); refEpoch is the right edge of every cgroup's window.
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return nil, fmt.Errorf("clock_gettime: %w", err)
	}
	refEpoch := (uint64(ts.Sec)*1_000_000_000 + uint64(ts.Nsec)) / TimelineBucketNs

	var (
		key     uint64
		percpu  []schedSchedBuckets
		results []CgroupTimeline
		keys    []uint64
	)

	it := m.Iterate()
	for it.Next(&key, &percpu) {
		tl := CgroupTimeline{
			CgroupID:  key,
			RunqLatNs: make([]uint64, TimelineBuckets),
			RunqCount: make([]uint64, TimelineBuckets),
			CpuNs:     make([]uint64, TimelineBuckets),
			CtxSwitch: make([]uint64, TimelineBuckets),
		}
		for _, cpu := range percpu {
			for _, b := range cpu.B {
				if b.Epoch == 0 || b.Epoch > refEpoch {
					continue // never written, or ahead of now (stale wrap)
				}
				age := refEpoch - b.Epoch
				if age >= TimelineBuckets {
					continue // older than the ring spans
				}
				pos := TimelineBuckets - 1 - int(age) // oldest→newest
				tl.RunqLatNs[pos] += b.RunqLatNs
				tl.RunqCount[pos] += uint64(b.RunqCount)
				tl.CpuNs[pos] += b.CpuNs
				tl.CtxSwitch[pos] += uint64(b.CtxSwitches)
			}
		}
		results = append(results, tl)
		keys = append(keys, key)
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("iterate sched_timeline_map: %w", err)
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
