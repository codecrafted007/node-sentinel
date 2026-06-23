// Command controller is the node-sentinel cluster controller. It aggregates the
// per-node contention snapshots that agents POST to it and prints a cluster-wide
// view, and — when --remediate is set (issue #7) — acts on confident offenders
// by emitting Kubernetes Events. Observe-only by default. Pure Go, builds
// anywhere; the Kubernetes client is only constructed when remediation is on.
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/codecrafted007/node-sentinel/internal/controller"
)

func main() {
	listen := flag.String("listen", ":8080", "address agents report to (POST /report)")
	logInterval := flag.Duration("log-interval", 10*time.Second, "how often to print the cluster summary")
	staleAfter := flag.Duration("stale-after", 30*time.Second, "mark a node stale (DataGap) if no report within this")
	remediate := flag.Bool("remediate", false, "act on confident offenders (emit Events). Off = observe-only")
	dryRun := flag.Bool("dry-run", false, "with --remediate, log intended actions but make no API calls")
	cooldown := flag.Duration("cooldown", 5*time.Minute, "minimum time between actions on the same pod")
	kubeconfig := flag.String("kubeconfig", "", "path to a kubeconfig (default: in-cluster config)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := controller.New(*staleAfter)

	if *remediate {
		client, err := k8sClient(*kubeconfig)
		if err != nil {
			log.Fatalf("controller: --remediate needs a Kubernetes client: %v", err)
		}
		c.WithRemediator(controller.NewRemediator(client, controller.RemediationConfig{
			DryRun:   *dryRun,
			Cooldown: *cooldown,
		}))
		log.Printf("node-sentinel controller: remediation ENABLED (dry-run=%v, cooldown=%s)", *dryRun, *cooldown)
	} else {
		log.Printf("node-sentinel controller: observe-only (pass --remediate to act)")
	}

	go c.LogLoop(ctx, *logInterval)

	log.Printf("node-sentinel controller: listening on %s (POST /report, GET /status, /healthz)", *listen)
	if err := c.Serve(ctx, *listen); err != nil && ctx.Err() == nil {
		log.Fatalf("controller: %v", err)
	}
}

// k8sClient builds a clientset from an explicit kubeconfig, or the in-cluster
// service-account config when run as a pod.
func k8sClient(kubeconfig string) (kubernetes.Interface, error) {
	var (
		cfg *rest.Config
		err error
	)
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}
