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
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := agent.New(cfg).Run(ctx); err != nil {
		log.Fatalf("agent: %v", err)
	}
}
