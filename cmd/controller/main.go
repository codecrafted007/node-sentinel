// Command controller is the node-sentinel cluster controller. Slice 1: it
// aggregates the per-node contention snapshots that agents POST to it and prints
// a cluster-wide view. No Kubernetes API, no remediation yet — those come in
// later slices. Pure Go, builds anywhere.
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/codecrafted007/node-sentinal/internal/controller"
)

func main() {
	listen := flag.String("listen", ":8080", "address agents report to (POST /report)")
	logInterval := flag.Duration("log-interval", 10*time.Second, "how often to print the cluster summary")
	staleAfter := flag.Duration("stale-after", 30*time.Second, "mark a node stale (DataGap) if no report within this")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := controller.New(*staleAfter)
	go c.LogLoop(ctx, *logInterval)

	log.Printf("node-sentinel controller: listening on %s (POST /report, GET /status, /healthz)", *listen)
	if err := c.Serve(ctx, *listen); err != nil && ctx.Err() == nil {
		log.Fatalf("controller: %v", err)
	}
}
