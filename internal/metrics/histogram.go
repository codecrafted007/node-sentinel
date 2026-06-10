// Package metrics turns the raw log2 histograms collected in-kernel by the eBPF
// programs into percentile estimates. It is pure Go with no Linux dependency so
// it can be unit-tested on any platform.
package metrics

import "math"

// MaxSlots is the number of log2 buckets in a histogram. Slot i counts events
// whose value falls in [2^i, 2^(i+1)). With values measured in microseconds,
// 27 slots cover the range from <1µs up to ~134 seconds, which is far beyond
// any realistic run-queue or I/O latency.
const MaxSlots = 27

// Percentile estimates the p-th percentile (p in [0,100]) from a log2 histogram.
// The returned value is in the same unit as the histogram was recorded in
// (microseconds for the scheduler observer).
//
// Because buckets are log2-spaced we cannot recover an exact value; we return
// the midpoint of the bucket the percentile falls into (2^i * 1.5). This yields
// roughly 2x precision — sufficient for comparing against coarse thresholds
// (e.g. 20ms / 50ms / 100ms) without paying for a dense histogram.
func Percentile(slots []uint64, p float64) float64 {
	var total uint64
	for _, c := range slots {
		total += c
	}
	if total == 0 {
		return 0
	}

	// Number of events at or below the target percentile.
	target := uint64(math.Ceil(p / 100 * float64(total)))
	if target == 0 {
		target = 1
	}

	var cum uint64
	for i, c := range slots {
		cum += c
		if cum >= target {
			return bucketMidpoint(i)
		}
	}
	return bucketMidpoint(len(slots) - 1)
}

// Mean returns the arithmetic mean recorded alongside the histogram. total is
// the summed raw value (e.g. total_us) and count the number of events.
func Mean(total, count uint64) float64 {
	if count == 0 {
		return 0
	}
	return float64(total) / float64(count)
}

// bucketMidpoint returns the representative value for bucket i: the midpoint of
// the half-open range [2^i, 2^(i+1)), i.e. 2^i * 1.5.
func bucketMidpoint(i int) float64 {
	return math.Pow(2, float64(i)) * 1.5
}
