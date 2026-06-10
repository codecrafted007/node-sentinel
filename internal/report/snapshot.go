// Package report holds the agent's contention snapshot — the shared view that
// stdout, the Prometheus endpoint, and sentinelctl all render. It has no
// dependencies so any of them can import it cheaply.
package report

// Snapshot is the agent's judgement for one interval: healthy unless at least
// one pod is genuinely starved of CPU.
type Snapshot struct {
	Time        string     `json:"time"`
	Healthy     bool       `json:"healthy"`
	CgroupsSeen int        `json:"cgroups_seen"`
	RunqWarnUs  float64    `json:"runq_warn_us"`
	MinSamples  int        `json:"min_samples"`
	Offenders   []Offender `json:"offenders"`
	Victims     []Victim   `json:"victims"`
}

// Offender is a cgroup ranked by CPU time, judged against its fair share.
type Offender struct {
	Pod       string  `json:"pod"`
	CPUms     float64 `json:"cpu_ms"`
	Intensity float64 `json:"intensity"` // percent of CPU consumed (0-100)
	ReqMilli  int64   `json:"req_milli"` // CPU request in millicores; -1 = unattributed/system
	Verdict   string  `json:"verdict"`
}

// Victim is a pod waiting on the run queue (high run-queue latency).
type Victim struct {
	Pod    string  `json:"pod"`
	P50us  float64 `json:"runq_p50_us"`
	P99us  float64 `json:"runq_p99_us"`
	Events uint64  `json:"events"`
}
