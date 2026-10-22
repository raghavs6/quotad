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
		r.Critical("core", w, w, w.Add(100*time.Millisecond))
	}
	// One call wanted at 0s never finished; the run ended at 30s.
	r.Critical("core", start, time.Time{}, time.Time{})

	s := r.Summarize(start.Add(30 * time.Second))
	if s.CriticalDone != 9 || s.CriticalUnfinished != 1 {
		t.Fatalf("done %d unfinished %d, want 9, 1", s.CriticalDone, s.CriticalUnfinished)
	}
	if s.CriticalP95 != 30 {
		t.Fatalf("p95 = %v, want 30 (the stuck call counted at run end)", s.CriticalP95)
	}
}

func TestWaitsSplitByResource(t *testing.T) {
	start := time.Unix(1000, 0)
	r := NewRecorder(start)
	ms := func(n int) time.Time { return start.Add(time.Duration(n) * time.Millisecond) }
	// Core: wanted at 0, stuck behind the agent's search until 2000, then
	// done 100ms after it was sent.
	r.Critical("core", ms(0), ms(2000), ms(2100))
	// Search: sent at once, held 2s.
	r.Critical("search", ms(0), ms(0), ms(2000))
	// Search: sent at 5000, still held when the run ends at 10000.
	r.Critical("search", ms(4000), ms(5000), time.Time{})
	// Wanted but never sent, resource unknown: totals only.
	r.Critical("", ms(9000), time.Time{}, time.Time{})

	s := r.Summarize(ms(10000))
	if s.CriticalDone != 2 || s.CriticalUnfinished != 2 {
		t.Fatalf("totals: done %d unfinished %d, want 2, 2", s.CriticalDone, s.CriticalUnfinished)
	}
	if len(s.Resources) != 2 {
		t.Fatalf("resources = %v, want core and search only", s.Resources)
	}
	core := s.Resources["core"]
	if core.Done != 1 || core.Unfinished != 0 || core.WaitP95 != 2.1 || core.CallP95 != 0.1 {
		t.Errorf("core = %+v, want 1 done, wait 2.1s, call 0.1s", core)
	}
	search := s.Resources["search"]
	if search.Done != 1 || search.Unfinished != 1 || search.WaitP95 != 6 || search.CallP50 != 2 || search.CallP95 != 5 {
		t.Errorf("search = %+v, want 1 done, 1 unfinished, wait p95 6s, call p50 2s, p95 5s", search)
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
