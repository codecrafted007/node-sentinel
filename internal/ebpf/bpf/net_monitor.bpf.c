// SPDX-License-Identifier: GPL-2.0
//
// net_monitor — network contention observer.
//
// Per cgroup it counts TCP retransmissions (the victim signal — a pod whose
// packets keep being retransmitted is suffering network trouble) and TX bytes
// (the offender signal — a pod flooding the NIC).
//
// Unlike CPU and disk, network events often fire in softirq / timer context, so
// the current task is not the socket's owner. We read the cgroup from the socket
// instead — sk->sk_cgrp_data.cgroup is the cgroup that created the socket.
//
//   tp_btf/tcp_retransmit_skb — a TCP segment is being retransmitted
//   fentry/tcp_sendmsg        — bytes handed to TCP for sending

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

#define MAX_CGROUPS 4096

struct net_stats {
	__u64 retransmits; /* TCP segments retransmitted */
	__u64 tx_bytes;    /* bytes handed to TCP for sending */
	__u64 tx_segs;     /* number of sendmsg calls */
};

/* cgroup id -> network counters. Per-CPU; user space sums on read. */
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_HASH);
	__uint(max_entries, MAX_CGROUPS);
	__type(key, __u64);
	__type(value, struct net_stats);
} net_stats_map SEC(".maps");

/* Look up (or create) the stats slot for a cgroup. */
static __always_inline struct net_stats *get_stats(__u64 cgroup_id)
{
	struct net_stats *s = bpf_map_lookup_elem(&net_stats_map, &cgroup_id);

	if (!s) {
		struct net_stats zero = {};

		bpf_map_update_elem(&net_stats_map, &cgroup_id, &zero, BPF_NOEXIST);
		s = bpf_map_lookup_elem(&net_stats_map, &cgroup_id);
	}
	return s;
}

/* The cgroup that owns this socket (read from the sock, not from current). */
static __always_inline __u64 sock_cgroup_id(struct sock *sk)
{
	return BPF_CORE_READ(sk, sk_cgrp_data.cgroup, kn, id);
}

/* A TCP segment is being retransmitted — charge the victim signal. */
SEC("tp_btf/tcp_retransmit_skb")
int BPF_PROG(handle_tcp_retransmit, struct sock *sk, struct sk_buff *skb)
{
	struct net_stats *s = get_stats(sock_cgroup_id(sk));

	if (s)
		__sync_fetch_and_add(&s->retransmits, 1);
	return 0;
}

/* Bytes handed to TCP for sending — charge the throughput (offender) signal. */
SEC("fentry/tcp_sendmsg")
int BPF_PROG(handle_tcp_sendmsg, struct sock *sk, struct msghdr *msg, __u64 size)
{
	struct net_stats *s = get_stats(sock_cgroup_id(sk));

	if (s) {
		__sync_fetch_and_add(&s->tx_bytes, size);
		__sync_fetch_and_add(&s->tx_segs, 1);
	}
	return 0;
}

char LICENSE[] SEC("license") = "GPL";
