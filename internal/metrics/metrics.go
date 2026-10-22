// Package metrics records what simulated agents saw and turns it into the
// step 1 numbers: critical-call waits, rejections over time, and ranges
// across repeated runs.
package metrics

import (
	"math"
	"slices"
	"sync"
	"time"
)

// Percentile returns the nearest-rank p-th percentile (0 < p <= 100) of xs.
// It returns 0 for an empty slice. xs is not modified.
func Percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	rank := int(math.Ceil(p / 100 * float64(len(s))))
	return s[max(rank, 1)-1]
}

// Recorder collects events from many goroutines. Use NewRecorder.
type Recorder struct {
	start time.Time

	mu       sync.Mutex
	rejected []time.Time // when a 403 or 429 came back
	resets   map[int64]bool
	waits    []call // critical calls only
}

type call struct {
	resource           string    // "core" or "search": the budget the call spends
	wanted, sent, done time.Time // sent is zero if the call was never sent, done if it never succeeded
}

// NewRecorder starts a recorder whose run began at start.
func NewRecorder(start time.Time) *Recorder {
	return &Recorder{start: start, resets: map[int64]bool{}}
}

// Rejection records a 403 or 429 received at t.
func (r *Recorder) Rejection(t time.Time) {
	r.mu.Lock()
	r.rejected = append(r.rejected, t)
	r.mu.Unlock()
}

// Reset records a window reset time an agent saw in x-ratelimit-reset.
func (r *Recorder) Reset(unix int64) {
	r.mu.Lock()
	r.resets[unix] = true
	r.mu.Unlock()
}

// Critical records one critical call: which resource it spends, when the
// agent wanted to send it, when the agent first sent it, and when it
// succeeded. Pass a zero sent for a call the agent never got to send, and a
// zero done for a call still unfinished at the end.
func (r *Recorder) Critical(resource string, wanted, sent, done time.Time) {
	r.mu.Lock()
	r.waits = append(r.waits, call{resource, wanted, sent, done})
	r.mu.Unlock()
}

// Summary is one run's numbers from the agents' side.
type Summary struct {
	CriticalDone       int     `json:"critical_done"`
	CriticalUnfinished int     `json:"critical_unfinished"`
	CriticalP50        float64 `json:"critical_p50_s"`
	CriticalP95        float64 `json:"critical_p95_s"`
	Rejections         int     `json:"rejections"`
	PeakRejectionsPerS int     `json:"peak_rejections_per_s"`
	PostResetShare     float64 `json:"post_reset_share"` // of rejections, the share within PostResetSpan after a reset
	RejectionsPerS     []int   `json:"rejections_per_s"` // second i of the run

	// The critical calls again, split by the resource they spend.
	Resources map[string]ResourceSummary `json:"resources,omitempty"`
}

// ResourceSummary is the critical calls of one resource.
//
// Wait runs from when the agent wanted the call to when it succeeded, as
// for all critical calls. It includes time the agent spent stuck on its
// own earlier calls, which may be of another resource: an agent sends one
// task call at a time. Call time runs from when the agent first sent the
// call to when it succeeded. It is only what happened to this call: a hold
// in quotad, rejections and retries, and the server's latency. A call in
// flight at the end counts with a call time of end minus sent; a call never
// sent has no call time.
type ResourceSummary struct {
	Done       int     `json:"done"`
	Unfinished int     `json:"unfinished"`
	WaitP50    float64 `json:"wait_p50_s"`
	WaitP95    float64 `json:"wait_p95_s"`
	CallP50    float64 `json:"call_p50_s"`
	CallP95    float64 `json:"call_p95_s"`
}

// PostResetSpan is how soon after a reset a rejection counts as part of the
// post-reset rush.
const PostResetSpan = 2 * time.Second

// Summarize computes the run's numbers. A critical call that never
// succeeded counts with a wait of end minus when it was wanted, so a run
// where calls get stuck can't report a better p95 by leaving them out.
func (r *Recorder) Summarize(end time.Time) Summary {
	r.mu.Lock()
	defer r.mu.Unlock()

	var s Summary
	type samples struct {
		ResourceSummary
		waits, calls []float64
	}
	all := &samples{}
	byResource := map[string]*samples{}
	for _, c := range r.waits {
		// A call with no resource counts only in the totals.
		res := &samples{}
		if c.resource != "" {
			if byResource[c.resource] == nil {
				byResource[c.resource] = &samples{}
			}
			res = byResource[c.resource]
		}
		done := c.done
		for _, x := range []*samples{all, res} {
			if done.IsZero() {
				x.Unfinished++
			} else {
				x.Done++
			}
		}
		if done.IsZero() {
			done = end
		}
		all.waits = append(all.waits, done.Sub(c.wanted).Seconds())
		res.waits = append(res.waits, done.Sub(c.wanted).Seconds())
		if !c.sent.IsZero() {
			res.calls = append(res.calls, done.Sub(c.sent).Seconds())
		}
	}
	s.CriticalDone, s.CriticalUnfinished = all.Done, all.Unfinished
	s.CriticalP50 = Percentile(all.waits, 50)
	s.CriticalP95 = Percentile(all.waits, 95)
	if len(byResource) > 0 {
		s.Resources = map[string]ResourceSummary{}
	}
	for name, x := range byResource {
		x.WaitP50, x.WaitP95 = Percentile(x.waits, 50), Percentile(x.waits, 95)
		x.CallP50, x.CallP95 = Percentile(x.calls, 50), Percentile(x.calls, 95)
		s.Resources[name] = x.ResourceSummary
	}

	s.Rejections = len(r.rejected)
	s.RejectionsPerS = make([]int, int(math.Ceil(end.Sub(r.start).Seconds())))
	nearReset := 0
	for _, t := range r.rejected {
		if i := int(t.Sub(r.start) / time.Second); i >= 0 && i < len(s.RejectionsPerS) {
			s.RejectionsPerS[i]++
		}
		if r.afterReset(t) {
			nearReset++
		}
	}
	for _, n := range s.RejectionsPerS {
		s.PeakRejectionsPerS = max(s.PeakRejectionsPerS, n)
	}
	if s.Rejections > 0 {
		s.PostResetShare = float64(nearReset) / float64(s.Rejections)
	}
	return s
}

func (r *Recorder) afterReset(t time.Time) bool {
	for unix := range r.resets {
		since := t.Sub(time.Unix(unix, 0))
		if since >= 0 && since < PostResetSpan {
			return true
		}
	}
	return false
}

// Range is the spread of one number across repeated runs.
type Range struct {
	Mean float64 `json:"mean"`
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
}

// RangeOf summarizes xs. It returns the zero Range for an empty slice.
func RangeOf(xs []float64) Range {
	if len(xs) == 0 {
		return Range{}
	}
	r := Range{Min: xs[0], Max: xs[0]}
	sum := 0.0
	for _, x := range xs {
		sum += x
		r.Min = min(r.Min, x)
		r.Max = max(r.Max, x)
	}
	r.Mean = sum / float64(len(xs))
	return r
}

// Overlaps reports whether two ranges share any value. Per the bench rules,
// a difference counts only when the ranges don't overlap.
func (a Range) Overlaps(b Range) bool {
	return a.Min <= b.Max && b.Min <= a.Max
}
