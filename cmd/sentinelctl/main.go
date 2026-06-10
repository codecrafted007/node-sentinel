// Command sentinelctl is the on-node CLI for the node-sentinel agent. It reads
// the agent's live snapshot over its unix socket.
//
//	sentinelctl top      live, refreshing view of contention (default)
//	sentinelctl status   one-shot snapshot
//
// Run it on the node (e.g. kubectl exec into the agent pod). Pure Go, no eBPF.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/codecrafted007/node-sentinal/internal/report"
)

const defaultSocket = "/var/run/sentinel/agent.sock"

func main() {
	socket := flag.String("socket", defaultSocket, "agent unix socket")
	interval := flag.Duration("interval", 2*time.Second, "refresh interval for top")
	flag.Parse()

	client := unixClient(*socket)

	switch flag.Arg(0) {
	case "", "top":
		runTop(client, *interval)
	case "status":
		snap, err := fetch(client)
		if err != nil {
			fail(err)
		}
		render(snap)
	default:
		fmt.Fprintln(os.Stderr, "usage: sentinelctl [top|status] [--socket path] [--interval d]")
		os.Exit(2)
	}
}

// unixClient is an HTTP client that dials the agent's unix socket. The URL host
// is ignored; only the path matters.
func unixClient(socket string) *http.Client {
	return &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
	}
}

func fetch(c *http.Client) (report.Snapshot, error) {
	var snap report.Snapshot
	resp, err := c.Get("http://unix/snapshot")
	if err != nil {
		return snap, err
	}
	defer resp.Body.Close()
	err = json.NewDecoder(resp.Body).Decode(&snap)
	return snap, err
}

func runTop(c *http.Client, interval time.Duration) {
	for {
		fmt.Print("\033[H\033[2J") // clear screen
		if snap, err := fetch(c); err != nil {
			fmt.Printf("sentinelctl: cannot reach agent (%v)\n", err)
		} else {
			render(snap)
		}
		time.Sleep(interval)
	}
}

func render(s report.Snapshot) {
	if s.Time == "" {
		fmt.Println("no data yet — the agent has not completed an interval")
		return
	}
	if s.Healthy {
		fmt.Printf("node-sentinel  %s   [OK] HEALTHY — no CPU contention (%d cgroups; threshold p99>=%.0fus, >=%d samples)\n",
			s.Time, s.CgroupsSeen, s.RunqWarnUs, s.MinSamples)
		return
	}

	fmt.Printf("node-sentinel  %s   [!] CPU CONTENTION — %d pod(s) starved\n\n", s.Time, len(s.Victims))

	fmt.Printf("OFFENDERS — by CPU time\n")
	fmt.Printf("%-44s %10s %10s %9s  %s\n", "POD", "CPU_MS", "INTENSITY", "REQ_mCPU", "VERDICT")
	for _, o := range s.Offenders {
		req := "-"
		if o.ReqMilli >= 0 {
			req = fmt.Sprintf("%d", o.ReqMilli)
		}
		fmt.Printf("%-44s %10.0f %9.1f%% %9s  %s\n", trunc(o.Pod, 44), o.CPUms, o.Intensity, req, o.Verdict)
	}

	fmt.Printf("\nVICTIMS — by run-queue latency\n")
	fmt.Printf("%-44s %12s %12s %10s\n", "POD", "RUNQ_P50_US", "RUNQ_P99_US", "EVENTS")
	for _, v := range s.Victims {
		fmt.Printf("%-44s %12.0f %12.0f %10d\n", trunc(v.Pod, 44), v.P50us, v.P99us, v.Events)
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "sentinelctl: %v\n", err)
	os.Exit(1)
}
