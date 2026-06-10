// SPDX-License-Identifier: GPL-2.0
//
// sched_monitor — CPU scheduling contention observer.
//
// It produces two signals, both from the scheduler's context-switch tracepoint:
//
//   * run-queue latency (the VICTIM signal): how long a task waited for a CPU
//     after being woken. High values mean a pod is being starved of CPU.
//
//   * on-CPU time (the OFFENDER signal): how much CPU time each cgroup actually
//     used. A pod using far more than its fair share is the noisy neighbour.
//
// Built with clang -target bpf using CO-RE, so one binary works across kernels.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

#define MAX_SLOTS   27    /* histogram buckets; keep in sync with metrics.MaxSlots */
#define MAX_CGROUPS 4096
#define MAX_TASKS   65536

/* A log2 histogram of run-queue latency (microseconds) for one cgroup. */
struct sched_hist {
	__u64 slots[MAX_SLOTS]; /* slots[i] counts waits in [2^i, 2^(i+1)) us */
	__u64 total_us;         /* sum of all waits, used for the mean */
	__u64 count;            /* number of waits */
};

/* cgroup id -> run-queue latency histogram.
 * Per-CPU so the hot context-switch path never waits on a lock; user space
 * adds up the per-CPU copies when it reads the map. */
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_HASH);
	__uint(max_entries, MAX_CGROUPS);
	__type(key, __u64);
	__type(value, struct sched_hist);
} runq_latency_map SEC(".maps");

/* pid -> time the task was woken (nanoseconds).
 * Not per-CPU: a task can wake on one CPU and start running on another. */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_TASKS);
	__type(key, __u32);
	__type(value, __u64);
} wakeup_ts_map SEC(".maps");

/* cgroup id -> CPU time used this interval (nanoseconds). Per-CPU. */
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_HASH);
	__uint(max_entries, MAX_CGROUPS);
	__type(key, __u64);
	__type(value, __u64);
} cpu_time_map SEC(".maps");

/* One slot per CPU: the time the task now running on this CPU started running.
 * At the next switch, (now - start) is the slice the outgoing task just ran. */
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, __u64);
} cpu_slice_start SEC(".maps");

/* Return the histogram bucket for a value: floor(log2(value)), capped to the
 * last slot. Example: 800 -> 9, because 2^9 (512) <= 800 < 2^10 (1024). */
static __always_inline __u64 log2_bucket(__u64 value)
{
	__u64 bucket = 0;

	while (value > 1 && bucket < MAX_SLOTS - 1) {
		value >>= 1;
		bucket++;
	}
	return bucket;
}

/* Remember when a task was woken, so the next switch can measure its wait. */
static __always_inline void save_wakeup_time(__u32 pid)
{
	__u64 now = bpf_ktime_get_ns();

	bpf_map_update_elem(&wakeup_ts_map, &pid, &now, BPF_ANY);
}

/* Add an on-CPU slice (nanoseconds) to a cgroup's running total. */
static __always_inline void add_cpu_time(__u64 cgroup_id, __u64 nanoseconds)
{
	__u64 *total = bpf_map_lookup_elem(&cpu_time_map, &cgroup_id);

	if (!total) {
		__u64 zero = 0;

		bpf_map_update_elem(&cpu_time_map, &cgroup_id, &zero, BPF_NOEXIST);
		total = bpf_map_lookup_elem(&cpu_time_map, &cgroup_id);
		if (!total)
			return;
	}
	__sync_fetch_and_add(total, nanoseconds);
}

/* A task became runnable: remember the time it was woken. */
SEC("tp_btf/sched_wakeup")
int BPF_PROG(handle_sched_wakeup, struct task_struct *task)
{
	save_wakeup_time(BPF_CORE_READ(task, pid));
	return 0;
}

/* A new task became runnable: same as a wakeup. */
SEC("tp_btf/sched_wakeup_new")
int BPF_PROG(handle_sched_wakeup_new, struct task_struct *task)
{
	save_wakeup_time(BPF_CORE_READ(task, pid));
	return 0;
}

/* A context switch: `prev` is leaving the CPU, `next` is taking it. */
SEC("tp_btf/sched_switch")
int BPF_PROG(handle_sched_switch, bool preempt, struct task_struct *prev,
	     struct task_struct *next)
{
	__u64 now = bpf_ktime_get_ns();
	__u32 index = 0;

	/* OFFENDER signal: charge the slice that just ended to `prev`. */
	__u64 *slice_start = bpf_map_lookup_elem(&cpu_slice_start, &index);

	if (slice_start) {
		__u32 prev_pid = BPF_CORE_READ(prev, pid);

		/* Skip the first switch (start is 0) and the idle task (pid 0);
		 * idle time is not a pod's CPU usage. */
		if (*slice_start != 0 && prev_pid != 0) {
			__u64 cgroup_id = BPF_CORE_READ(prev, cgroups, dfl_cgrp, kn, id);

			add_cpu_time(cgroup_id, now - *slice_start);
		}
		*slice_start = now;
	}

	/* VICTIM signal: how long did `next` wait in the run queue? */
	__u32 next_pid = BPF_CORE_READ(next, pid);
	__u64 *wakeup_time = bpf_map_lookup_elem(&wakeup_ts_map, &next_pid);

	if (!wakeup_time)
		return 0;

	__u64 wait_us = (now - *wakeup_time) / 1000;

	bpf_map_delete_elem(&wakeup_ts_map, &next_pid);

	__u64 cgroup_id = BPF_CORE_READ(next, cgroups, dfl_cgrp, kn, id);
	struct sched_hist *hist = bpf_map_lookup_elem(&runq_latency_map, &cgroup_id);

	if (!hist) {
		struct sched_hist empty = {};

		bpf_map_update_elem(&runq_latency_map, &cgroup_id, &empty, BPF_NOEXIST);
		hist = bpf_map_lookup_elem(&runq_latency_map, &cgroup_id);
		if (!hist)
			return 0;
	}

	__u64 bucket = log2_bucket(wait_us);

	if (bucket >= MAX_SLOTS)
		bucket = MAX_SLOTS - 1;

	__sync_fetch_and_add(&hist->slots[bucket], 1);
	__sync_fetch_and_add(&hist->total_us, wait_us);
	__sync_fetch_and_add(&hist->count, 1);
	return 0;
}

char LICENSE[] SEC("license") = "GPL";
