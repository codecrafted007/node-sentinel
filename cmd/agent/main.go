//go:build linux

// Command agent is the node-sentinel per-node agent. Phase 1: loads the
// scheduler observer, resolves cgroups to pods, and prints live per-pod
// run-queue latency. Per design §7.2.1 this file is just entry point, flag
// parsing, and signal handling; the lifecycle lives in internal/agent.
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"github.com/codecrafted007/node-sentinal/internal/agent"
)

func main() {
	cfg := agent.DefaultConfig()
	flag.DurationVar(&cfg.ReadInterval, "interval", cfg.ReadInterval, "map read interval")
	flag.IntVar(&cfg.TopN, "top", cfg.TopN, "number of cgroups to display")
	flag.StringVar(&cfg.CRISocket, "cri-socket", cfg.CRISocket, "CRI endpoint for pod resolution")
	flag.StringVar(&cfg.CgroupRoot, "cgroup-root", cfg.CgroupRoot, "cgroups v2 subtree to scan for pods")
	flag.IntVar(&cfg.MinSamples, "min-samples", cfg.MinSamples, "min run-queue samples before a pod counts as a victim")
	flag.DurationVar(&cfg.RunqWarn, "runq-warn", cfg.RunqWarn, "run-queue p99 a pod must exceed to count as contention")
	flag.Float64Var(&cfg.DeviationFactor, "deviation", cfg.DeviationFactor, "x over a pod's own baseline p99 to count as a victim (once warm)")
	flag.Float64Var(&cfg.ConfidenceThreshold, "confidence", cfg.ConfidenceThreshold, "offender confidence needed to name a pod the noisy neighbour")
	flag.DurationVar(&cfg.IOWarn, "io-warn", cfg.IOWarn, "disk I/O p99 latency a pod must exceed to count as an I/O victim")
	flag.IntVar(&cfg.MinOps, "min-ops", cfg.MinOps, "min completed I/O requests before a cgroup's I/O p99 is trusted")
	flag.StringVar(&cfg.MetricsAddr, "metrics-addr", cfg.MetricsAddr, "Prometheus /metrics listen address (empty to disable)")
	flag.StringVar(&cfg.LocalSocket, "local-socket", cfg.LocalSocket, "unix socket for sentinelctl (empty to disable)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := agent.New(cfg).Run(ctx); err != nil {
		log.Fatalf("agent: %v", err)
	}
}
