// SPDX-License-Identifier: GPL-2.0
//
// blkio_monitor — block I/O contention observer.
//
// Measures, per cgroup, how long disk I/O requests take and how many bytes they
// move. A pod saturating the disk makes its neighbours' I/O wait — the disk
// equivalent of the CPU run-queue signal.
//
//   block_rq_insert   — a request enters the device queue: remember its start
//                       time, size, and the cgroup that issued it
//   block_rq_complete — the request finishes: latency = now - start, charged to
//                       that cgroup
//
// Note: buffered writes are flushed later by kernel threads, so they attribute
// to the kernel/root cgroup rather than the originating pod. Reads and direct /
// synchronous writes attribute correctly.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

#define MAX_SLOTS    27 /* keep in sync with metrics.MaxSlots */
#define MAX_CGROUPS  4096
#define MAX_INFLIGHT 65536

/* Per-cgroup I/O latency histogram plus throughput counters. */
struct blkio_hist {
	__u64 slots[MAX_SLOTS]; /* log2 histogram of I/O latency (µs) */
	__u64 total_us;         /* summed latency, for the mean */
	__u64 count;            /* number of completed requests */
	__u64 bytes;            /* total bytes moved */
};

/* cgroup id -> I/O latency + throughput. Per-CPU; user space sums on read. */
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_HASH);
	__uint(max_entries, MAX_CGROUPS);
	__type(key, __u64);
	__type(value, struct blkio_hist);
} blkio_latency_map SEC(".maps");

/* In-flight request -> who issued it and when. Keyed by the request pointer. */
struct rq_start {
	__u64 ts;
	__u64 cgroup_id;
	__u32 bytes;
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_INFLIGHT);
	__type(key, __u64);
	__type(value, struct rq_start);
} inflight_rq SEC(".maps");

static __always_inline __u64 log2_bucket(__u64 value)
{
	__u64 bucket = 0;

	while (value > 1 && bucket < MAX_SLOTS - 1) {
		value >>= 1;
		bucket++;
	}
	return bucket;
}

/* A request enters the device queue: stamp it with who issued it and when. */
SEC("tp_btf/block_rq_insert")
int BPF_PROG(handle_block_rq_insert, struct request *rq)
{
	struct rq_start start = {};

	start.ts = bpf_ktime_get_ns();
	start.cgroup_id = bpf_get_current_cgroup_id();
	start.bytes = BPF_CORE_READ(rq, __data_len);

	__u64 key = (__u64)(unsigned long)rq;

	bpf_map_update_elem(&inflight_rq, &key, &start, BPF_ANY);
	return 0;
}

/* The request finishes: charge its latency and bytes to the issuing cgroup. */
SEC("tp_btf/block_rq_complete")
int BPF_PROG(handle_block_rq_complete, struct request *rq, int error, unsigned int nr_bytes)
{
	__u64 key = (__u64)(unsigned long)rq;
	struct rq_start *start = bpf_map_lookup_elem(&inflight_rq, &key);

	if (!start)
		return 0;

	__u64 delta_us = (bpf_ktime_get_ns() - start->ts) / 1000;
	__u64 cgroup_id = start->cgroup_id;
	__u32 bytes = start->bytes;

	bpf_map_delete_elem(&inflight_rq, &key);

	struct blkio_hist *hist = bpf_map_lookup_elem(&blkio_latency_map, &cgroup_id);

	if (!hist) {
		struct blkio_hist empty = {};

		bpf_map_update_elem(&blkio_latency_map, &cgroup_id, &empty, BPF_NOEXIST);
		hist = bpf_map_lookup_elem(&blkio_latency_map, &cgroup_id);
		if (!hist)
			return 0;
	}

	__u64 bucket = log2_bucket(delta_us);

	if (bucket >= MAX_SLOTS)
		bucket = MAX_SLOTS - 1;

	__sync_fetch_and_add(&hist->slots[bucket], 1);
	__sync_fetch_and_add(&hist->total_us, delta_us);
	__sync_fetch_and_add(&hist->count, 1);
	__sync_fetch_and_add(&hist->bytes, bytes);
	return 0;
}

char LICENSE[] SEC("license") = "GPL";
