package server

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metric descriptors. Per-pod series are emitted only for the pods currently in
// the snapshot (the top offenders/victims), so cardinality is bounded and a
// healthy node emits just the two node-level gauges.
var (
	descContended = prometheus.NewDesc("sentinel_node_contended",
		"1 if the node is currently CPU-contended, else 0", nil, nil)
	descCgroups = prometheus.NewDesc("sentinel_cgroups_observed",
		"cgroups seen in the last interval", nil, nil)
	descIntensity = prometheus.NewDesc("sentinel_pod_cpu_intensity_ratio",
		"offender pod's share of CPU consumed (0-1)", []string{"pod"}, nil)
	descCPUms = prometheus.NewDesc("sentinel_pod_cpu_milliseconds",
		"offender pod's on-CPU milliseconds in the last interval", []string{"pod"}, nil)
	descRunqP99 = prometheus.NewDesc("sentinel_pod_runqueue_p99_microseconds",
		"victim pod's run-queue p99 latency", []string{"pod"}, nil)
	descRunqP50 = prometheus.NewDesc("sentinel_pod_runqueue_p50_microseconds",
		"victim pod's run-queue p50 latency", []string{"pod"}, nil)
	descMaxConfidence = prometheus.NewDesc("sentinel_max_offender_confidence",
		"highest offender confidence this interval (0-1; -1 if none attributable)", nil, nil)
	descConfidence = prometheus.NewDesc("sentinel_pod_offender_confidence",
		"confidence that an offender pod is the noisy neighbour (0-1)", []string{"pod"}, nil)
	descDegradation = prometheus.NewDesc("sentinel_pod_runqueue_degradation",
		"victim pod's run-queue p99 relative to its own baseline", []string{"pod"}, nil)
	descIOBytes = prometheus.NewDesc("sentinel_pod_io_bytes",
		"offender pod's disk bytes this interval", []string{"pod"}, nil)
	descIOLatP99 = prometheus.NewDesc("sentinel_pod_io_latency_p99_microseconds",
		"I/O-victim pod's disk latency p99", []string{"pod"}, nil)
	descIOConfidence = prometheus.NewDesc("sentinel_pod_io_offender_confidence",
		"confidence that a pod is the disk noisy neighbour (0-1)", []string{"pod"}, nil)
	descIOMaxConfidence = prometheus.NewDesc("sentinel_max_io_offender_confidence",
		"highest disk-I/O offender confidence this interval (-1 if none attributable)", nil, nil)
	descNetTxBytes = prometheus.NewDesc("sentinel_pod_net_tx_bytes",
		"offender pod's TCP TX bytes this interval", []string{"pod"}, nil)
	descNetRetransmits = prometheus.NewDesc("sentinel_pod_net_retransmits",
		"victim pod's TCP retransmits this interval", []string{"pod"}, nil)
	descNetConfidence = prometheus.NewDesc("sentinel_pod_net_offender_confidence",
		"confidence that a pod is the network noisy neighbour (0-1)", []string{"pod"}, nil)
	descNetMaxConfidence = prometheus.NewDesc("sentinel_max_net_offender_confidence",
		"highest network offender confidence this interval (-1 if none attributable)", nil, nil)
)

// collector emits metrics from the latest snapshot at scrape time, so series
// for pods that are no longer hot disappear instead of leaking forever.
type collector struct{ store *Store }

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descContended
	ch <- descCgroups
	ch <- descIntensity
	ch <- descCPUms
	ch <- descRunqP99
	ch <- descRunqP50
	ch <- descMaxConfidence
	ch <- descConfidence
	ch <- descDegradation
	ch <- descIOBytes
	ch <- descIOLatP99
	ch <- descIOConfidence
	ch <- descIOMaxConfidence
	ch <- descNetTxBytes
	ch <- descNetRetransmits
	ch <- descNetConfidence
	ch <- descNetMaxConfidence
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	s := c.store.Get()

	contended := 0.0
	if !s.Healthy {
		contended = 1
	}
	ch <- prometheus.MustNewConstMetric(descContended, prometheus.GaugeValue, contended)
	ch <- prometheus.MustNewConstMetric(descCgroups, prometheus.GaugeValue, float64(s.CgroupsSeen))
	ch <- prometheus.MustNewConstMetric(descMaxConfidence, prometheus.GaugeValue, s.MaxConfidence)

	seen := map[string]bool{}
	for _, o := range s.Offenders {
		if seen[o.Pod] {
			continue // guard against a duplicate label set breaking the scrape
		}
		seen[o.Pod] = true
		ch <- prometheus.MustNewConstMetric(descIntensity, prometheus.GaugeValue, o.Intensity/100, o.Pod)
		ch <- prometheus.MustNewConstMetric(descCPUms, prometheus.GaugeValue, o.CPUms, o.Pod)
		if o.Confidence >= 0 {
			ch <- prometheus.MustNewConstMetric(descConfidence, prometheus.GaugeValue, o.Confidence, o.Pod)
		}
	}

	seen = map[string]bool{}
	for _, v := range s.Victims {
		if seen[v.Pod] {
			continue
		}
		seen[v.Pod] = true
		ch <- prometheus.MustNewConstMetric(descRunqP99, prometheus.GaugeValue, v.P99us, v.Pod)
		ch <- prometheus.MustNewConstMetric(descRunqP50, prometheus.GaugeValue, v.P50us, v.Pod)
		if v.Degradation > 0 {
			ch <- prometheus.MustNewConstMetric(descDegradation, prometheus.GaugeValue, v.Degradation, v.Pod)
		}
	}

	ch <- prometheus.MustNewConstMetric(descIOMaxConfidence, prometheus.GaugeValue, s.IOMaxConfidence)

	seen = map[string]bool{}
	for _, o := range s.IOOffenders {
		if seen[o.Pod] {
			continue
		}
		seen[o.Pod] = true
		ch <- prometheus.MustNewConstMetric(descIOBytes, prometheus.GaugeValue, o.MB*1e6, o.Pod)
		if o.Confidence >= 0 {
			ch <- prometheus.MustNewConstMetric(descIOConfidence, prometheus.GaugeValue, o.Confidence, o.Pod)
		}
	}

	seen = map[string]bool{}
	for _, v := range s.IOVictims {
		if seen[v.Pod] {
			continue
		}
		seen[v.Pod] = true
		ch <- prometheus.MustNewConstMetric(descIOLatP99, prometheus.GaugeValue, v.P99us, v.Pod)
	}

	ch <- prometheus.MustNewConstMetric(descNetMaxConfidence, prometheus.GaugeValue, s.NetMaxConfidence)

	seen = map[string]bool{}
	for _, o := range s.NetOffenders {
		if seen[o.Pod] {
			continue
		}
		seen[o.Pod] = true
		ch <- prometheus.MustNewConstMetric(descNetTxBytes, prometheus.GaugeValue, o.MB*1e6, o.Pod)
		if o.Confidence >= 0 {
			ch <- prometheus.MustNewConstMetric(descNetConfidence, prometheus.GaugeValue, o.Confidence, o.Pod)
		}
	}

	seen = map[string]bool{}
	for _, v := range s.NetVictims {
		if seen[v.Pod] {
			continue
		}
		seen[v.Pod] = true
		ch <- prometheus.MustNewConstMetric(descNetRetransmits, prometheus.GaugeValue, float64(v.Retransmits), v.Pod)
	}
}

// ServeMetrics serves Prometheus /metrics plus /healthz and /readyz on addr,
// backed by store. Blocks until ctx is cancelled.
func ServeMetrics(ctx context.Context, addr string, store *Store) error {
	reg := prometheus.NewRegistry()
	reg.MustRegister(&collector{store: store})

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }
	mux.HandleFunc("/healthz", ok)
	mux.HandleFunc("/readyz", ok)

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	return srv.ListenAndServe()
}
