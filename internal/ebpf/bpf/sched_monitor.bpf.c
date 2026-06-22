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

/* Sub-interval timeline ring (issue #4): a coarse time dimension so user space
 * can correlate an offender's CPU bursts against a victim's run-queue stalls in
 * the SAME 100ms window, instead of averaging both away over a 5s scrape. */
#define NUM_BUCKETS        50            /* 50 x 100ms = a rolling 5s timeline */
#define BUCKET_NS          100000000ULL  /* 100ms sub-interval width */
#define MAX_ACTIVE_CGROUPS 512           /* realistic active cgroups/node; bounds map memory */

/* A log2 histogram of run-queue latency (microseconds) for one cgroup. */
struct sched_hist {
	__u64 slots[MAX_SLOTS]; /* slots[i] counts waits in [2^i, 2^(i+1)) us */
	__u64 total_us;         /* sum of all waits, used for the mean */
	__u64 count;            /* number of waits */
};

/* One 100ms time bucket for a cgroup (issue #4). A ring of these gives the
 * coarse timeline the user-space correlation scorer needs (see
 * internal/metrics/correlation.go + docs/sim/temporal-correlation.html #2). */
struct sched_bucket {
	__u64 epoch;        /* absolute window number (ktime / BUCKET_NS) this slot holds */
	__u64 runq_lat_ns;  /* VICTIM: summed run-queue wait in this window */
	__u64 cpu_ns;       /* OFFENDER: on-CPU time charged in this window */
	__u32 runq_count;   /* run-queue waits counted (activity-gate input) */
	__u32 ctx_switches; /* on-CPU slices charged (activity-gate input) */
};

/* A fixed ring of NUM_BUCKETS windows = one cgroup's last ~5s, as a map value. */
struct sched_buckets {
	struct sched_bucket b[NUM_BUCKETS];
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

/* cgroup id -> rolling 5s timeline of (run-queue, cpu) per 100ms bucket.
 * Per-CPU and lock-free like the histograms; user space drains and re-aligns
 * the per-CPU copies by epoch each interval. max_entries is sized to a
 * realistic active-cgroup count (NOT MAX_CGROUPS) to bound memory on high-core
 * nodes, since cost is per-CPU x NUM_BUCKETS x sizeof(bucket) x max_entries. */
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_HASH);
	__uint(max_entries, MAX_ACTIVE_CGROUPS);
	__type(key, __u64);
	__type(value, struct sched_buckets);
} sched_timeline_map SEC(".maps");

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

/* Return the time bucket for `cgroup_id` covering the 100ms window that
 * contains `now`, lazily resetting the slot if it still holds an older window.
 *
 * This is how one fixed ring of NUM_BUCKETS slots represents a sliding 5s
 * timeline: each slot remembers which absolute window (epoch) it currently
 * holds. When the ring wraps and we land on a slot from a previous revolution,
 * its epoch won't match, so we zero it before use — discarding the stale window
 * instead of mixing two windows' counts together. */
static __always_inline struct sched_bucket *
get_timeline_bucket(__u64 cgroup_id, __u64 now)
{
	struct sched_buckets *bs = bpf_map_lookup_elem(&sched_timeline_map, &cgroup_id);

	if (!bs) {
		struct sched_buckets empty = {};

		bpf_map_update_elem(&sched_timeline_map, &cgroup_id, &empty, BPF_NOEXIST);
		bs = bpf_map_lookup_elem(&sched_timeline_map, &cgroup_id);
		if (!bs)
			return 0;
	}

	__u64 epoch = now / BUCKET_NS;
	__u32 idx = epoch % NUM_BUCKETS;

	if (idx >= NUM_BUCKETS) /* tell the verifier the index is in range */
		return 0;

	struct sched_bucket *bkt = &bs->b[idx];

	if (bkt->epoch != epoch) {
		bkt->epoch = epoch; /* lazy reset: this slot held an older window */
		bkt->runq_lat_ns = 0;
		bkt->cpu_ns = 0;
		bkt->runq_count = 0;
		bkt->ctx_switches = 0;
	}
	return bkt;
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
			__u64 slice_ns = now - *slice_start;

			add_cpu_time(cgroup_id, slice_ns);

			/* OFFENDER, time-resolved: charge this slice to the 100ms bucket. */
			struct sched_bucket *bkt = get_timeline_bucket(cgroup_id, now);

			if (bkt) {
				__sync_fetch_and_add(&bkt->cpu_ns, slice_ns);
				__sync_fetch_and_add(&bkt->ctx_switches, 1);
			}
		}
		*slice_start = now;
	}

	/* VICTIM signal: how long did `next` wait in the run queue? */
	__u32 next_pid = BPF_CORE_READ(next, pid);
	__u64 *wakeup_time = bpf_map_lookup_elem(&wakeup_ts_map, &next_pid);

	if (!wakeup_time)
		return 0;

	__u64 wait_ns = now - *wakeup_time;
	__u64 wait_us = wait_ns / 1000;

	bpf_map_delete_elem(&wakeup_ts_map, &next_pid);

	__u64 cgroup_id = BPF_CORE_READ(next, cgroups, dfl_cgrp, kn, id);

	/* VICTIM, time-resolved: add this wait to the current 100ms bucket. */
	struct sched_bucket *bkt = get_timeline_bucket(cgroup_id, now);

	if (bkt) {
		__sync_fetch_and_add(&bkt->runq_lat_ns, wait_ns);
		__sync_fetch_and_add(&bkt->runq_count, 1);
	}

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
