// Command controller is the node-sentinel cluster controller. It aggregates the
// per-node contention snapshots that agents POST to it and prints a cluster-wide
// view, and — when enabled (issue #7) — acts on confident offenders: an in-place
// /resize CPU throttle (restored after a window), falling back to a Kubernetes
// Event. Remediation is configured either by a NodeHealthPolicy CRD (--policy)
// or by flags (--remediate …). Observe-only by default. Pure Go, builds anywhere;
// the Kubernetes client is only constructed when remediation is on.
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/codecrafted007/node-sentinel/internal/controller"
)

func main() {
	listen := flag.String("listen", ":8080", "address agents report to (POST /report)")
	logInterval := flag.Duration("log-interval", 10*time.Second, "how often to print the cluster summary")
	staleAfter := flag.Duration("stale-after", 30*time.Second, "mark a node stale (DataGap) if no report within this")
	policyMode := flag.Bool("policy", false, "drive remediation from a NodeHealthPolicy CRD (overrides the remediation flags)")
	remediate := flag.Bool("remediate", false, "act on confident offenders via flags. Off = observe-only")
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

	switch {
	case *policyMode:
		configureFromPolicy(ctx, c, *kubeconfig)
	case *remediate:
		var nsList []string
		if *namespaces != "" {
			nsList = strings.Split(*namespaces, ",")
		}
		client, err := k8sClient(*kubeconfig)
		if err != nil {
			log.Fatalf("controller: --remediate needs a Kubernetes client: %v", err)
		}
		rem := controller.NewRemediator(client, controller.RemediationConfig{
			DryRun: *dryRun, Cooldown: *cooldown, Resize: *resize,
			RestoreAfter: *restoreAfter, Namespaces: nsList,
		})
		c.WithRemediator(rem)
		if *resize {
			go rem.RunRestore(ctx)
		}
		log.Printf("node-sentinel controller: remediation ENABLED via flags (dry-run=%v, resize=%v, cooldown=%s, restore-after=%s)",
			*dryRun, *resize, *cooldown, *restoreAfter)
	default:
		log.Printf("node-sentinel controller: observe-only (pass --policy or --remediate to act)")
	}

	go c.LogLoop(ctx, *logInterval)

	log.Printf("node-sentinel controller: listening on %s (POST /report, GET /status, /healthz)", *listen)
	if err := c.Serve(ctx, *listen); err != nil && ctx.Err() == nil {
		log.Fatalf("controller: %v", err)
	}
}

// configureFromPolicy loads the active NodeHealthPolicy at startup and configures
// the controller from it. (Live re-watch is a follow-up; today a policy change
// needs a controller restart.)
func configureFromPolicy(ctx context.Context, c *controller.Controller, kubeconfig string) {
	cfg, err := restConfig(kubeconfig)
	if err != nil {
		log.Fatalf("controller: --policy needs a Kubernetes client: %v", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("controller: build clientset: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("controller: build dynamic client: %v", err)
	}

	p, ok, err := controller.LoadActivePolicy(ctx, dyn)
	if err != nil {
		log.Printf("node-sentinel controller: could not load NodeHealthPolicy (%v) → observe-only", err)
		return
	}
	if !ok {
		log.Printf("node-sentinel controller: --policy set but no NodeHealthPolicy found → observe-only")
		return
	}

	rcfg, active := p.ToConfig()
	if !active {
		log.Printf("node-sentinel controller: %s → observe-only", p.Describe())
		return
	}
	rem := controller.NewRemediator(client, rcfg)
	c.WithRemediator(rem)
	if rcfg.Resize {
		go rem.RunRestore(ctx)
	}
	log.Printf("node-sentinel controller: %s → remediation ACTIVE (resize=%v, cooldown=%s, restore-after=%s, namespaces=%v)",
		p.Describe(), rcfg.Resize, rcfg.Cooldown, rcfg.RestoreAfter, rcfg.Namespaces)
}

// restConfig builds a rest.Config from an explicit kubeconfig, or the in-cluster
// service-account config when run as a pod.
func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

// k8sClient builds a clientset (flag-driven remediation path).
func k8sClient(kubeconfig string) (kubernetes.Interface, error) {
	cfg, err := restConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}
