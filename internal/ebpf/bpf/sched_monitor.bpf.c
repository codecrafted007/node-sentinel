// SPDX-License-Identifier: GPL-2.0
//
// sched_monitor — CPU scheduling contention observer (Phase 1, minimal).
//
// Measures run-queue latency: the time a task spends runnable-but-not-running,
// i.e. how long it waited for a CPU after being woken. This is the core
// "noisy neighbor" CPU signal — a pod can be within its CPU limit yet still
// make its neighbors wait in the run queue.
//
// Approach (mirrors bcc/libbpf-tools runqlat):
//   sched_wakeup / sched_wakeup_new → stamp wakeup time for the task's pid
//   sched_switch                    → on the incoming task, delta = now - wakeup,
//                                     bucket into a per-cgroup log2 histogram
//
// CO-RE via tp_btf raw tracepoints (kernel 5.5+). Built with clang -target bpf.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

#define MAX_SLOTS 27 /* keep in sync with metrics.MaxSlots */
#define MAX_CGROUPS 4096
#define MAX_TASKS 65536

struct sched_hist {
	__u64 slots[MAX_SLOTS]; /* log2 histogram of run-queue latency (µs) */
	__u64 total_us;         /* summed latency, for mean */
	__u64 count;            /* number of events */
};

// Per-cgroup run-queue latency. PERCPU so the high-frequency sched_switch path
// never contends a lock; userspace sums the per-CPU copies on read.
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_HASH);
	__uint(max_entries, MAX_CGROUPS);
	__type(key, __u64); /* cgroup_id */
	__type(value, struct sched_hist);
} runq_latency_map SEC(".maps");

// Transient wakeup timestamps, keyed by pid. Not per-CPU: a task can wake on
// one CPU and be scheduled on another.
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_TASKS);
	__type(key, __u32);  /* pid */
	__type(value, __u64); /* wakeup timestamp (ns) */
} wakeup_ts_map SEC(".maps");

static __always_inline __u64 log2_u32(__u32 v)
{
	__u32 shift, r;
	r = (v > 0xFFFF) << 4; v >>= r;
	shift = (v > 0xFF) << 3; v >>= shift; r |= shift;
	shift = (v > 0xF) << 2; v >>= shift; r |= shift;
	shift = (v > 0x3) << 1; v >>= shift; r |= shift;
	r |= (v >> 1);
	return r;
}

static __always_inline __u64 log2_u64(__u64 v)
{
	__u32 hi = v >> 32;
	if (hi)
		return log2_u32(hi) + 32;
	return log2_u32(v);
}

static __always_inline void stamp_wakeup(__u32 pid)
{
	__u64 ts = bpf_ktime_get_ns();
	bpf_map_update_elem(&wakeup_ts_map, &pid, &ts, BPF_ANY);
}

SEC("tp_btf/sched_wakeup")
int BPF_PROG(handle_sched_wakeup, struct task_struct *p)
{
	stamp_wakeup(BPF_CORE_READ(p, pid));
	return 0;
}

SEC("tp_btf/sched_wakeup_new")
int BPF_PROG(handle_sched_wakeup_new, struct task_struct *p)
{
	stamp_wakeup(BPF_CORE_READ(p, pid));
	return 0;
}

SEC("tp_btf/sched_switch")
int BPF_PROG(handle_sched_switch, bool preempt, struct task_struct *prev,
	     struct task_struct *next)
{
	__u32 pid = BPF_CORE_READ(next, pid);

	__u64 *tsp = bpf_map_lookup_elem(&wakeup_ts_map, &pid);
	if (!tsp)
		return 0;

	__u64 delta_us = (bpf_ktime_get_ns() - *tsp) / 1000;
	bpf_map_delete_elem(&wakeup_ts_map, &pid);

	// cgroups v2 id of the task that just got the CPU.
	__u64 cgid = BPF_CORE_READ(next, cgroups, dfl_cgrp, kn, id);

	struct sched_hist *hp = bpf_map_lookup_elem(&runq_latency_map, &cgid);
	if (!hp) {
		struct sched_hist zero = {};
		bpf_map_update_elem(&runq_latency_map, &cgid, &zero, BPF_NOEXIST);
		hp = bpf_map_lookup_elem(&runq_latency_map, &cgid);
		if (!hp)
			return 0;
	}

	__u64 slot = log2_u64(delta_us);
	if (slot >= MAX_SLOTS)
		slot = MAX_SLOTS - 1;

	__sync_fetch_and_add(&hp->slots[slot], 1);
	__sync_fetch_and_add(&hp->total_us, delta_us);
	__sync_fetch_and_add(&hp->count, 1);
	return 0;
}

char LICENSE[] SEC("license") = "GPL";
