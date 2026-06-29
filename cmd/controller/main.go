// Command controller is the node-sentinel cluster controller. It aggregates the
// per-node contention snapshots that agents POST to it and prints a cluster-wide
// view, and — when --remediate is set (issue #7) — acts on confident offenders:
// an in-place /resize CPU throttle (with --resize) that is restored after a
// window, falling back to a Kubernetes Event. Observe-only by default. Pure Go,
// builds anywhere; the Kubernetes client is only constructed when remediation is on.
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"strings"
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
	remediate := flag.Bool("remediate", false, "act on confident offenders. Off = observe-only")
	dryRun := flag.Bool("dry-run", false, "with --remediate, log intended actions but make no API calls")
	cooldown := flag.Duration("cooldown", 5*time.Minute, "minimum time between actions on the same pod")
	resize := flag.Bool("resize", false, "with --remediate, throttle a CPU offender's limit via in-place /resize (else Event-only)")
	restoreAfter := flag.Duration("restore-after", 10*time.Minute, "how long a /resize throttle stays in place before it is restored")
	namespaces := flag.String("remediate-namespaces", "", "comma-separated namespaces to remediate in (empty = all)")
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
		var nsList []string
		if *namespaces != "" {
			nsList = strings.Split(*namespaces, ",")
		}
		rem := controller.NewRemediator(client, controller.RemediationConfig{
			DryRun:       *dryRun,
			Cooldown:     *cooldown,
			Resize:       *resize,
			RestoreAfter: *restoreAfter,
			Namespaces:   nsList,
		})
		c.WithRemediator(rem)
		if *resize {
			go rem.RunRestore(ctx) // lift elapsed throttles
		}
		scope := "all namespaces"
		if *namespaces != "" {
			scope = "namespaces=" + *namespaces
		}
		log.Printf("node-sentinel controller: remediation ENABLED (dry-run=%v, resize=%v, cooldown=%s, restore-after=%s, %s)",
			*dryRun, *resize, *cooldown, *restoreAfter, scope)
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
