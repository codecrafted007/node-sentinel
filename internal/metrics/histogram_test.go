package metrics

import (
	"math"
	"testing"
)

func slotsWith(buckets map[int]uint64) []uint64 {
	s := make([]uint64, MaxSlots)
	for i, c := range buckets {
		s[i] = c
	}
	return s
}

func TestPercentileEmpty(t *testing.T) {
	if got := Percentile(make([]uint64, MaxSlots), 99); got != 0 {
		t.Fatalf("empty histogram p99 = %v, want 0", got)
	}
}

func TestPercentileSingleBucket(t *testing.T) {
	// All 1000 events in bucket 5 → [32µs, 64µs). Every percentile is the
	// midpoint of bucket 5 = 2^5 * 1.5 = 48.
	s := slotsWith(map[int]uint64{5: 1000})
	for _, p := range []float64{1, 50, 99, 100} {
		if got := Percentile(s, p); got != 48 {
			t.Errorf("p%v = %v, want 48", p, got)
		}
	}
}

func TestPercentileSplitDistribution(t *testing.T) {
	// 90 events in bucket 3 ([8,16)µs), 10 events in bucket 10 ([1024,2048)µs).
	// p50 lands in bucket 3 (midpoint 12); p99 lands in bucket 10 (midpoint 1536).
	s := slotsWith(map[int]uint64{3: 90, 10: 10})

	if got, want := Percentile(s, 50), bucketMidpoint(3); got != want {
		t.Errorf("p50 = %v, want %v", got, want)
	}
	if got, want := Percentile(s, 99), bucketMidpoint(10); got != want {
		t.Errorf("p99 = %v, want %v", got, want)
	}
}

func TestMean(t *testing.T) {
	if got := Mean(0, 0); got != 0 {
		t.Errorf("Mean(0,0) = %v, want 0", got)
	}
	if got := Mean(1000, 8); got != 125 {
		t.Errorf("Mean(1000,8) = %v, want 125", got)
	}
}

func TestBucketMidpoint(t *testing.T) {
	if got := bucketMidpoint(0); got != 1.5 {
		t.Errorf("bucket 0 midpoint = %v, want 1.5", got)
	}
	if got, want := bucketMidpoint(20), math.Pow(2, 20)*1.5; got != want {
		t.Errorf("bucket 20 midpoint = %v, want %v", got, want)
	}
}
