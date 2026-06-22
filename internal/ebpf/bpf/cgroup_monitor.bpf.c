// SPDX-License-Identifier: GPL-2.0
//
// cgroup_monitor — cgroup lifecycle observer (issue #2).
//
// Captures cgroup creation and teardown the instant they happen, so a container
// born and torn down between user-space rescans is still named. The kernel side
// stays lean: record (cgroup_id, op, path) and emit it to a ring buffer; all the
// parsing and the CRI join happen in Go.
//
//   tp_btf/cgroup_mkdir — a cgroup directory was created (before any task enters)
//   tp_btf/cgroup_rmdir — a cgroup directory was removed

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

#define EVENT_PATH_LEN  256

#define CGROUP_OP_MKDIR 0
#define CGROUP_OP_RMDIR 1

/* One cgroup lifecycle event emitted to user space. */
struct cgroup_event {
	__u64 cgroup_id;            /* kernfs id == the cgroup_id our other maps key on */
	__u32 op;                   /* CGROUP_OP_MKDIR or CGROUP_OP_RMDIR */
	char  path[EVENT_PATH_LEN]; /* cgroup path; Go parses the container ID from it */
};

/* Force bpf2go to emit BTF for the event type: unlike the histogram structs it
 * is not a map value type (a ringbuf has none), so nothing else references it in
 * a way that would emit its BTF for the `-type cgroup_event` binding. */
struct cgroup_event *_unused_cgroup_event __attribute__((unused));

/* Lifecycle events, drained by a Go ringbuf reader. */
struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 256 * 1024); /* 256 KB */
} cgroup_events SEC(".maps");

/* Reserve an event, fill it, and submit it. path is a kernel string. */
static __always_inline void emit(__u64 cgroup_id, __u32 op, const char *path)
{
	struct cgroup_event *e = bpf_ringbuf_reserve(&cgroup_events, sizeof(*e), 0);

	if (!e)
		return; /* ring full — drop; the periodic rescan is the backstop */
	e->cgroup_id = cgroup_id;
	e->op = op;
	bpf_probe_read_kernel_str(e->path, sizeof(e->path), path);
	bpf_ringbuf_submit(e, 0);
}

SEC("tp_btf/cgroup_mkdir")
int BPF_PROG(handle_cgroup_mkdir, struct cgroup *cgrp, const char *path)
{
	emit(BPF_CORE_READ(cgrp, kn, id), CGROUP_OP_MKDIR, path);
	return 0;
}

SEC("tp_btf/cgroup_rmdir")
int BPF_PROG(handle_cgroup_rmdir, struct cgroup *cgrp, const char *path)
{
	emit(BPF_CORE_READ(cgrp, kn, id), CGROUP_OP_RMDIR, path);
	return 0;
}

char LICENSE[] SEC("license") = "GPL";
