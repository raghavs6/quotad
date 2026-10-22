// Package budget tracks one token's rate limits and decides which requests
// may go to the upstream now, which wait, and which are turned away.
//
// GitHub keeps a separate primary budget for each resource on a token
// (core, search, code_search, graphql, ...). The caller names the resource
// a request is expected to count against; the response's
// x-ratelimit-resource header says which one it really did.
//
// Four checks must all pass before a request is sent:
//   - Primary budget of its resource, learned from response headers
//     (x-ratelimit-*).
//   - In-flight cap: requests sent but not yet answered.
//   - Points per window (read 1, write 5). A request's points count from
//     when it is sent until a full window after its answer, so any stretch
//     of one window at the upstream stays under the cap.
//   - No pause: after a secondary-limit 429, every request on the token
//     waits until the upstream's retry-after has passed.
//
// GitHub sends the primary budget in every response but not the secondary
// limits, so those two caps are configured, defaulting to GitHub's numbers.
// The secondary limits and the pause belong to the whole token.
//
// Each resource has its own line, so a request waiting for one resource's
// reset never holds up another resource. When the token's shared limits
// free up, the oldest waiting request whose resource has budget goes first.
//
// The rule is reserve, then correct: a request takes one unit before it is
// sent, and the response's headers then correct the count. Within a window,
// GitHub's remaining count only goes down, so the smallest value seen for a
// window is the newest truth, whatever order responses arrive in.
package budget

import (
	"context"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Config sets how long requests may wait.
type Config struct {
	// MaxHold is the longest a request may wait for budget. A request that
	// would wait longer is turned away at once with ErrTooLong.
	MaxHold time.Duration
	// ResetGuard is extra time to wait past the upstream's reset time, in
	// case our clock runs ahead of the upstream's.
	ResetGuard time.Duration

	// MaxInFlight caps requests sent but not yet answered (GitHub: 100).
	MaxInFlight int
	// Points caps the cost of requests sent in any PointsWindow
	// (GitHub: 900 per minute).
	Points       int
	PointsWindow time.Duration
	// MinPause is how long to pause after a secondary-limit answer that has
	// no retry-after (GitHub: at least a minute). Each further such answer
	// in a row doubles it.
	MinPause time.Duration
}

// Point costs, from GitHub's secondary-limit rules.
const (
	ReadCost  = 1
	WriteCost = 5
)

// TooLongError means the request would wait longer than MaxHold.
type TooLongError struct {
	RetryAfter int // seconds until budget is expected back, at least 1
}

func (e *TooLongError) Error() string {
	return "budget: wait would exceed max hold; retry after " + strconv.Itoa(e.RetryAfter) + "s"
}

// ErrTooLong matches any *TooLongError with errors.Is.
var ErrTooLong = &TooLongError{}

func (e *TooLongError) Is(target error) bool { return target == ErrTooLong }

// Budget is one token's budget. It is safe for concurrent use.
type Budget struct {
	cfg      Config
	now      func() time.Time
	schedule func(time.Duration, func()) // runs f after d; time.AfterFunc in production

	mu        sync.Mutex
	resources map[string]*resource
	inFlight  int       // admitted requests not yet answered, across all resources
	arrivals  uint64    // counts requests as they arrive, to order the lines
	timerAt   time.Time // when the pending wake-up timer fires, to avoid duplicates

	spent  []*spend  // points of requests in flight or answered within the last PointsWindow
	paused time.Time // no request goes out before this (after a secondary-limit answer)
	streak int       // secondary-limit answers in a row without retry-after
}

// resource is one primary budget on the token, with its own line.
type resource struct {
	known     bool      // we have seen at least one response's headers
	probing   bool      // the one request sent to learn the budget is in flight
	limit     int       // x-ratelimit-limit
	remaining int       // x-ratelimit-remaining, lowest seen this window
	reset     time.Time // x-ratelimit-reset; zero after we roll into a window we haven't seen yet
	reserved  int       // admitted requests not yet answered
	waiting   []*waiter // held requests, oldest first
}

// spend is one request's points. They count from when it is sent until
// PointsWindow after its answer. The upstream counted it at some moment in
// between, and we can't know when, so the answer time is the safe end.
type spend struct {
	cost     int
	answered time.Time // zero while in flight
}

// close marks the request answered (or abandoned) now.
func (s *spend) close(now time.Time) {
	if s.answered.IsZero() {
		s.answered = now
	}
}

type waiter struct {
	arrival  uint64
	cost     int
	sp       *spend     // set when admitted
	deadline time.Time  // arrival + MaxHold
	done     chan error // nil error = admitted
}

// GitHub's published secondary limits, used for any Config field left at zero.
const (
	GitHubMaxInFlight = 100
	GitHubPoints      = 900
	GitHubPointsEvery = time.Minute
	GitHubMinPause    = time.Minute
)

// New returns a budget that knows nothing yet. Secondary-limit fields left
// at zero get GitHub's numbers. now and schedule are injected so tests
// control time; pass time.Now and AfterFunc for real use.
func New(cfg Config, now func() time.Time, schedule func(time.Duration, func())) *Budget {
	if cfg.MaxInFlight == 0 {
		cfg.MaxInFlight = GitHubMaxInFlight
	}
	if cfg.Points == 0 {
		cfg.Points = GitHubPoints
	}
	if cfg.PointsWindow == 0 {
		cfg.PointsWindow = GitHubPointsEvery
	}
	if cfg.MinPause == 0 {
		cfg.MinPause = GitHubMinPause
	}
	return &Budget{cfg: cfg, now: now, schedule: schedule, resources: map[string]*resource{}}
}

// resourceLocked returns the named resource, creating it on first use.
func (b *Budget) resourceLocked(name string) *resource {
	r := b.resources[name]
	if r == nil {
		r = &resource{}
		b.resources[name] = r
	}
	return r
}

// AfterFunc adapts time.AfterFunc to New's schedule parameter.
func AfterFunc(d time.Duration, f func()) { time.AfterFunc(d, f) }

// Acquire blocks until the request may be sent, ctx is done, or the wait
// would exceed MaxHold (a *TooLongError). res is the resource the request is
// expected to count against, such as "core" or "search". write says whether
// the request is a write, which costs more points. On success the caller
// must call exactly one of the Ticket's methods when the upstream answers
// or fails.
func (b *Budget) Acquire(ctx context.Context, res string, write bool) (*Ticket, error) {
	cost := ReadCost
	if write {
		cost = WriteCost
	}
	b.mu.Lock()
	r := b.resourceLocked(res)
	b.arrivals++
	w := &waiter{arrival: b.arrivals, cost: cost, deadline: b.now().Add(b.cfg.MaxHold), done: make(chan error, 1)}
	r.waiting = append(r.waiting, w)
	// Join the back of the line, then let the line move: w goes now only if
	// nobody who came earlier can go first.
	b.drainLocked()
	select {
	case err := <-w.done:
		b.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return &Ticket{b: b, r: r, sp: w.sp}, nil
	default:
	}
	b.schedule(b.cfg.MaxHold, b.drain)
	b.mu.Unlock()

	select {
	case err := <-w.done:
		if err != nil {
			return nil, err
		}
		return &Ticket{b: b, r: r, sp: w.sp}, nil
	case <-ctx.Done():
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, x := range r.waiting {
			if x == w {
				r.waiting = append(r.waiting[:i], r.waiting[i+1:]...)
				return nil, ctx.Err()
			}
		}
		// drain already answered w just as ctx ended. If it admitted w,
		// give the unit back so nothing is reserved for a request never sent.
		if err := <-w.done; err == nil {
			r.reserved--
			b.inFlight--
			w.sp.close(b.now()) // never sent; its points simply expire
			if !r.known {
				r.probing = false
			}
			b.drainLocked()
		}
		return nil, ctx.Err()
	}
}

// primaryReadyLocked reports whether r's primary budget lets one more
// request go now: as the probe if r is unknown, or because a unit is left.
func (r *resource) primaryReadyLocked() bool {
	if !r.known {
		return !r.probing
	}
	return !r.primaryOutLocked()
}

// sharedReadyLocked reports whether the token's shared checks (pause,
// in-flight cap, points) let a request of this cost go now.
func (b *Budget) sharedReadyLocked(now time.Time, cost int) bool {
	b.pruneLocked(now)
	return !now.Before(b.paused) && b.inFlight < b.cfg.MaxInFlight && b.pointsLocked()+cost <= b.cfg.Points
}

// admitLocked reserves one unit of r, one in-flight slot and cost points.
// The caller has checked that both primaryReadyLocked and sharedReadyLocked
// pass.
func (b *Budget) admitLocked(r *resource, cost int) *spend {
	if !r.known {
		r.probing = true // learn first: the rest wait for this one's headers
	}
	r.reserved++
	b.inFlight++
	sp := &spend{cost: cost}
	b.spent = append(b.spent, sp)
	return sp
}

// primaryOutLocked reports whether the primary budget is used up until the
// reset: every remaining unit is already reserved.
func (r *resource) primaryOutLocked() bool {
	return r.known && r.remaining-r.reserved <= 0
}

// pruneLocked forgets points of requests answered more than PointsWindow ago.
func (b *Budget) pruneLocked(now time.Time) {
	b.spent = slices.DeleteFunc(b.spent, func(s *spend) bool {
		return !s.answered.IsZero() && !s.answered.Add(b.cfg.PointsWindow).After(now)
	})
}

func (b *Budget) pointsLocked() int {
	n := 0
	for _, s := range b.spent {
		n += s.cost
	}
	return n
}

// rollLocked starts a new window once the old one's reset (plus the guard)
// has passed. We assume the full limit until a response says otherwise.
func (r *resource) rollLocked(now time.Time, guard time.Duration) {
	if r.known && !r.reset.IsZero() && !now.Before(r.reset.Add(guard)) {
		r.remaining = r.limit
		r.reset = time.Time{}
	}
}

// tooLongLocked reports whether a request that can't be sent now would
// wait past deadline. It refuses early only when a known wait (the primary
// reset, or a pause) already runs past the deadline; waits for in-flight
// answers or for points to expire are short, so those requests stay until
// their deadline.
func (b *Budget) tooLongLocked(r *resource, now, deadline time.Time) error {
	release := b.paused
	if r.primaryOutLocked() && !r.reset.IsZero() {
		release = laterOf(release, r.reset.Add(b.cfg.ResetGuard))
	}
	if release.After(deadline) {
		return &TooLongError{RetryAfter: ceilSeconds(release.Sub(now))}
	}
	if !now.Before(deadline) {
		return &TooLongError{RetryAfter: ceilSeconds(release.Sub(now))}
	}
	return nil
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// scheduleLocked makes sure held requests are looked at again at the next
// moment a check can start passing on its own: a held resource's primary
// reset, the end of a pause, or the oldest points leaving the window.
// (In-flight answers wake the lines themselves.)
func (b *Budget) scheduleLocked(now time.Time) {
	var at time.Time
	consider := func(t time.Time) {
		if t.After(now) && (at.IsZero() || t.Before(at)) {
			at = t
		}
	}
	for _, r := range b.resources {
		if len(r.waiting) > 0 && r.known && !r.reset.IsZero() {
			consider(r.reset.Add(b.cfg.ResetGuard))
		}
	}
	consider(b.paused)
	for _, s := range b.spent {
		if !s.answered.IsZero() {
			consider(s.answered.Add(b.cfg.PointsWindow))
		}
	}
	if at.IsZero() || (!b.timerAt.IsZero() && !at.Before(b.timerAt)) {
		return // nothing to wait for, or a timer at or before it is already set
	}
	b.timerAt = at
	b.schedule(at.Sub(now), func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		// Clear first: if the timer fired a hair early, drain reschedules it.
		b.timerAt = time.Time{}
		b.drainLocked()
	})
}

func (b *Budget) drain() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.drainLocked()
}

// drainLocked admits held requests while budget allows, and turns away any
// that can no longer be served within their max hold. Each resource's line
// keeps arrival order. Across lines, the oldest head whose resource has
// budget goes next; if the shared checks stop it, nobody behind it jumps
// ahead.
func (b *Budget) drainLocked() {
	now := b.now()
	for _, r := range b.resources {
		r.rollLocked(now, b.cfg.ResetGuard)
	}
	for {
		var next *resource
		for _, r := range b.resources {
			if len(r.waiting) > 0 && r.primaryReadyLocked() && (next == nil || r.waiting[0].arrival < next.waiting[0].arrival) {
				next = r
			}
		}
		if next == nil || !b.sharedReadyLocked(now, next.waiting[0].cost) {
			break
		}
		w := next.waiting[0]
		w.sp = b.admitLocked(next, w.cost)
		w.done <- nil
		next.waiting[0] = nil
		next.waiting = next.waiting[1:]
	}
	held := false
	for _, r := range b.resources {
		kept := r.waiting[:0]
		for _, w := range r.waiting {
			if err := b.tooLongLocked(r, now, w.deadline); err != nil {
				w.done <- err
				continue
			}
			kept = append(kept, w)
		}
		clear(r.waiting[len(kept):])
		r.waiting = kept
		held = held || len(kept) > 0
	}
	if held {
		b.scheduleLocked(now)
	}
}

// Ticket is one admitted request's reservation.
type Ticket struct {
	b    *Budget
	r    *resource // the resource it was admitted against
	sp   *spend
	once sync.Once
}

// Done corrects the budget from the upstream's response and releases the
// reservation. The headers correct the resource that x-ratelimit-resource
// names, which may differ from the one the request was admitted against;
// without that header, they correct the admitted one. A secondary-limit
// answer (a 429, or a 403 while primary budget remains) pauses every
// request on the token. A response without rate-limit headers counts as
// Failed, apart from any pause it asks for.
func (t *Ticket) Done(status int, h http.Header) {
	t.b.secondary(status, h)
	limit, err1 := strconv.Atoi(h.Get("x-ratelimit-limit"))
	remaining, err2 := strconv.Atoi(h.Get("x-ratelimit-remaining"))
	reset, err3 := strconv.ParseInt(h.Get("x-ratelimit-reset"), 10, 64)
	if errors.Join(err1, err2, err3) != nil {
		t.Failed()
		return
	}
	t.once.Do(func() {
		b := t.b
		b.mu.Lock()
		defer b.mu.Unlock()
		t.sp.close(b.now())
		t.r.reserved--
		t.r.probing = false
		b.inFlight--
		if status < 300 {
			b.streak = 0
		}
		r := t.r
		if name := h.Get("x-ratelimit-resource"); name != "" {
			r = b.resourceLocked(name)
		}
		r.known = true
		r.limit = limit
		switch rs := time.Unix(reset, 0); {
		case r.reset.IsZero() || rs.After(r.reset):
			// A window we haven't seen yet: take its count as is.
			r.reset = rs
			r.remaining = remaining
		case rs.Equal(r.reset):
			r.remaining = min(r.remaining, remaining)
		default:
			// A late answer from an older window says nothing about this one.
		}
		b.drainLocked()
	})
}

// Failed releases the reservation for a request whose upstream gave no
// usable answer (a timeout, a dropped connection). The upstream may still
// have counted it, so the unit stays spent until a response corrects us.
func (t *Ticket) Failed() {
	t.once.Do(func() {
		b := t.b
		b.mu.Lock()
		defer b.mu.Unlock()
		t.sp.close(b.now())
		t.r.reserved--
		b.inFlight--
		if t.r.known {
			t.r.remaining--
		} else {
			t.r.probing = false // let the next request try to learn the budget
		}
		b.drainLocked()
	})
}

// secondary pauses the token after a secondary-limit answer, following
// GitHub's order: use retry-after if given; if primary budget is at 0, the
// primary rules already hold that resource until its reset; otherwise wait
// MinPause, doubling for each such answer in a row.
func (b *Budget) secondary(status int, h http.Header) {
	remaining := h.Get("x-ratelimit-remaining")
	if status != http.StatusTooManyRequests && (status != http.StatusForbidden || remaining == "0" || remaining == "") {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	var wait time.Duration
	if s, err := strconv.Atoi(h.Get("retry-after")); err == nil && s > 0 {
		wait = time.Duration(s) * time.Second
	} else if remaining == "0" {
		return
	} else {
		wait = b.cfg.MinPause << min(b.streak, 10)
		b.streak++
	}
	b.paused = laterOf(b.paused, now.Add(wait))
	b.scheduleLocked(now)
}

func ceilSeconds(d time.Duration) int {
	return max(1, int(math.Ceil(d.Seconds())))
}
