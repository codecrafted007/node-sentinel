// This file is portable (no build tag): the cpu.stat parser is pure text
// handling, so it unit-tests on any OS. The cgroupfs read that feeds it lives in
// resolver.go (Linux only).
package cgroup

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// CPUStat holds the CFS throttling fields from a cgroup v2 cpu.stat file
// (issue #6). Throttling means a pod is hitting its CPU quota and backing up the
// scheduler — a cheap, strong offender signal that separates "efficiently busy"
// (high CPU, no throttle) from "disruptively bursty" (rising throttle).
//
// The throttling fields exist only when the cgroup has a CPU limit set; they are
// 0 when absent (no quota), which the parser handles by leaving them zero.
type CPUStat struct {
	NrPeriods     uint64 // nr_periods: CFS accounting periods elapsed
	NrThrottled   uint64 // nr_throttled: periods in which the cgroup was throttled
	ThrottledUsec uint64 // throttled_usec: total wall-time spent throttled
}

// ThrottledFraction returns the share of CFS periods in which the cgroup was
// throttled (0–1). Computed on per-interval deltas it answers "how often did
// this pod hit its cap recently"; 0 when no periods elapsed.
func (s CPUStat) ThrottledFraction() float64 {
	if s.NrPeriods == 0 {
		return 0
	}
	return float64(s.NrThrottled) / float64(s.NrPeriods)
}

// Sub returns the per-interval delta this − prev, clamped at 0 per field so a
// counter reset (pod restart, cgroup recreated) reads as no throttling rather
// than a huge negative wrap.
func (s CPUStat) Sub(prev CPUStat) CPUStat {
	return CPUStat{
		NrPeriods:     monoSub(s.NrPeriods, prev.NrPeriods),
		NrThrottled:   monoSub(s.NrThrottled, prev.NrThrottled),
		ThrottledUsec: monoSub(s.ThrottledUsec, prev.ThrottledUsec),
	}
}

func monoSub(cur, prev uint64) uint64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}

// parseCPUStat reads a cgroup v2 cpu.stat body ("key value" lines). Unknown keys
// and malformed lines are ignored, and missing throttling keys default to 0, so
// it is safe on cgroups with no quota.
func parseCPUStat(r io.Reader) (CPUStat, error) {
	var s CPUStat
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, val, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(val, 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "nr_periods":
			s.NrPeriods = n
		case "nr_throttled":
			s.NrThrottled = n
		case "throttled_usec":
			s.ThrottledUsec = n
		}
	}
	return s, sc.Err()
}
