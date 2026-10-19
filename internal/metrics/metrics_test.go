package metrics

import (
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	xs := []float64{5, 1, 4, 2, 3, 6, 7, 8, 9, 10}
	for _, tc := range []struct{ p, want float64 }{{50, 5}, {95, 10}, {10, 1}, {100, 10}} {
		if got := Percentile(xs, tc.p); got != tc.want {
			t.Errorf("p%v = %v, want %v", tc.p, got, tc.want)
		}
	}
	if xs[0] != 5 {
		t.Error("Percentile modified its input")
	}
	if Percentile(nil, 95) != 0 {
		t.Error("empty input should give 0")
	}
}

func TestUnfinishedCallsCountAtRunEnd(t *testing.T) {
	start := time.Unix(1000, 0)
	r := NewRecorder(start)
	for i := range 9 {
		w := start.Add(time.Duration(i) * time.Second)
		r.Critical(w, w.Add(100*time.Millisecond))
	}
	// One call wanted at 0s never finished; the run ended at 30s.
	r.Critical(start, time.Time{})

	s := r.Summarize(start.Add(30 * time.Second))
	if s.CriticalDone != 9 || s.CriticalUnfinished != 1 {
		t.Fatalf("done %d unfinished %d, want 9, 1", s.CriticalDone, s.CriticalUnfinished)
	}
	if s.CriticalP95 != 30 {
		t.Fatalf("p95 = %v, want 30 (the stuck call counted at run end)", s.CriticalP95)
	}
}

func TestRejectionsPerSecondAndPostReset(t *testing.T) {
	start := time.Unix(1000, 0)
	r := NewRecorder(start)
	r.Reset(1005)
	r.Rejection(start.Add(500 * time.Millisecond))  // second 0, not near a reset
	r.Rejection(start.Add(5100 * time.Millisecond)) // second 5, just after reset
	r.Rejection(start.Add(5200 * time.Millisecond))
	r.Rejection(start.Add(6900 * time.Millisecond)) // second 6, still inside 2s
	r.Rejection(start.Add(4900 * time.Millisecond)) // just before the reset

	s := r.Summarize(start.Add(10 * time.Second))
	if len(s.RejectionsPerS) != 10 || s.RejectionsPerS[0] != 1 || s.RejectionsPerS[5] != 2 || s.RejectionsPerS[6] != 1 {
		t.Fatalf("per second = %v", s.RejectionsPerS)
	}
	if s.PeakRejectionsPerS != 2 {
		t.Fatalf("peak = %d, want 2", s.PeakRejectionsPerS)
	}
	if s.PostResetShare != 0.6 {
		t.Fatalf("post-reset share = %v, want 0.6", s.PostResetShare)
	}
}

func TestRangeOverlap(t *testing.T) {
	a := RangeOf([]float64{1, 2, 3})
	if a != (Range{Mean: 2, Min: 1, Max: 3}) {
		t.Fatalf("range = %+v", a)
	}
	if !a.Overlaps(Range{Min: 3, Max: 5}) {
		t.Error("touching ranges should overlap")
	}
	if a.Overlaps(Range{Min: 3.5, Max: 5}) {
		t.Error("separate ranges should not overlap")
	}
}
