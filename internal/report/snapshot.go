// Package report holds the agent's contention snapshot — the shared view that
// stdout, the Prometheus endpoint, and sentinelctl all render. It has no
// dependencies so any of them can import it cheaply.
package report

// Snapshot is the agent's judgement for one interval: healthy unless at least
// one pod is genuinely starved of CPU.
type Snapshot struct {
	NodeName      string     `json:"node_name"`
	Time          string     `json:"time"`
	Healthy       bool       `json:"healthy"`
	CgroupsSeen   int        `json:"cgroups_seen"`
	RunqWarnUs    float64    `json:"runq_warn_us"`
	MinSamples    int        `json:"min_samples"`
	ConfidenceMin float64    `json:"confidence_min"` // confidence needed to call a pod the offender
	MaxConfidence float64    `json:"max_confidence"` // highest CPU offender confidence this interval (0-1, -1 if none attributable)
	Offenders     []Offender `json:"offenders"`      // CPU offenders
	Victims       []Victim   `json:"victims"`        // CPU victims (run-queue latency)

	// Disk I/O dimension (empty when there's no I/O contention).
	IOMaxConfidence float64      `json:"io_max_confidence"`
	IOOffenders     []IOOffender `json:"io_offenders"`
	IOVictims       []Victim     `json:"io_victims"` // P50/P99 are I/O latency µs; Events are ops

	// Network dimension (empty when there's no network contention).
	NetMaxConfidence float64       `json:"net_max_confidence"`
	NetOffenders     []NetOffender `json:"net_offenders"`
	NetVictims       []NetVictim   `json:"net_victims"`
}

// NetOffender is a cgroup ranked by network TX throughput.
type NetOffender struct {
	Pod        string  `json:"pod"`
	MB         float64 `json:"mb"`
	SharePct   float64 `json:"share"`
	Segs       uint64  `json:"segs"`
	Confidence float64 `json:"confidence"` // -1 = not attributable
}

// NetVictim is a pod whose TCP segments are being retransmitted.
type NetVictim struct {
	Pod         string  `json:"pod"`
	Retransmits uint64  `json:"retransmits"`
	RatePct     float64 `json:"rate"`        // retransmits / sendmsg calls
	Degradation float64 `json:"degradation"` // vs its own baseline; 0 = not warm
	Segs        uint64  `json:"segs"`
}

// IOOffender is a cgroup ranked by disk throughput — the disk equivalent of a
// CPU offender.
type IOOffender struct {
	Pod        string  `json:"pod"`
	MB         float64 `json:"mb"`
	SharePct   float64 `json:"share"`      // percent of disk bytes this interval
	Ops        uint64  `json:"ops"`
	Confidence float64 `json:"confidence"` // 0-1; -1 = not attributable
}

// Offender is a cgroup ranked by CPU time, judged against its fair share.
type Offender struct {
	Pod        string  `json:"pod"`
	CPUms      float64 `json:"cpu_ms"`
	Intensity  float64 `json:"intensity"`  // percent of CPU consumed (0-100)
	ReqMilli   int64   `json:"req_milli"`  // CPU request in millicores; -1 = unattributed/system
	Confidence float64 `json:"confidence"` // 0-1 that this pod is the noisy neighbour; -1 = not attributable
	Verdict    string  `json:"verdict"`
}

// Victim is a pod waiting on the run queue (high run-queue latency).
type Victim struct {
	Pod         string  `json:"pod"`
	P50us       float64 `json:"runq_p50_us"`
	P99us       float64 `json:"runq_p99_us"`
	Degradation float64 `json:"degradation"` // current p99 / its own baseline; 0 = baseline not warm yet
	Events      uint64  `json:"events"`
}
