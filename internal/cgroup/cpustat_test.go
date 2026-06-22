package cgroup

import (
	"strings"
	"testing"
)

func TestParseCPUStat(t *testing.T) {
	// A throttled cgroup (has a CPU quota) — real cgroup v2 cpu.stat shape.
	in := `usage_usec 168780000
user_usec 120000000
system_usec 48780000
nr_periods 5000
nr_throttled 1200
throttled_usec 8400000
core_sched.force_idle_usec 0`

	s, err := parseCPUStat(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.NrPeriods != 5000 || s.NrThrottled != 1200 || s.ThrottledUsec != 8400000 {
		t.Fatalf("got %+v", s)
	}
	if got, want := s.ThrottledFraction(), 1200.0/5000.0; got != want {
		t.Errorf("ThrottledFraction = %v, want %v", got, want)
	}
}

func TestParseCPUStatNoQuota(t *testing.T) {
	// A cgroup with no CPU limit: throttling keys are absent → all zero, and
	// ThrottledFraction must not divide by zero.
	in := "usage_usec 500\nuser_usec 300\nsystem_usec 200\n"
	s, err := parseCPUStat(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.NrPeriods != 0 || s.NrThrottled != 0 || s.ThrottledUsec != 0 {
		t.Errorf("expected zeros, got %+v", s)
	}
	if got := s.ThrottledFraction(); got != 0 {
		t.Errorf("ThrottledFraction = %v, want 0", got)
	}
}

func TestParseCPUStatMalformed(t *testing.T) {
	// Blank lines, a key with no value, and a non-numeric value are all skipped
	// rather than failing the whole parse.
	in := "\nnr_periods\nnr_throttled notanumber\nthrottled_usec 42\n"
	s, err := parseCPUStat(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.ThrottledUsec != 42 || s.NrThrottled != 0 || s.NrPeriods != 0 {
		t.Errorf("got %+v", s)
	}
}

func TestCPUStatSubMonotonic(t *testing.T) {
	cur := CPUStat{NrPeriods: 5200, NrThrottled: 1300, ThrottledUsec: 9000000}
	prev := CPUStat{NrPeriods: 5000, NrThrottled: 1200, ThrottledUsec: 8400000}
	d := cur.Sub(prev)
	if d.NrPeriods != 200 || d.NrThrottled != 100 || d.ThrottledUsec != 600000 {
		t.Fatalf("delta got %+v", d)
	}

	// Counter reset (pod restarted, cgroup recreated): cur < prev must clamp to 0.
	reset := CPUStat{NrPeriods: 10, NrThrottled: 0, ThrottledUsec: 0}.Sub(prev)
	if reset.NrPeriods != 0 || reset.NrThrottled != 0 || reset.ThrottledUsec != 0 {
		t.Errorf("counter reset must clamp to 0, got %+v", reset)
	}
}
