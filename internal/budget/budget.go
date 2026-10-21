// Package budget tracks one token's rate limits and decides which requests
// may go to the upstream now, which wait, and which are turned away.
//
// Four checks must all pass before a request is sent:
//   - Primary budget, learned from response headers (x-ratelimit-*).
//   - In-flight cap: requests sent but not yet answered.
//   - Points per window, counted over a sliding window (read 1, write 5).
//   - No pause: after a secondary-limit 429, every request on the token
//     waits until the upstream's retry-after has passed.
//
// GitHub sends the primary budget in every response but not the secondary
// limits, so those two caps are configured, defaulting to GitHub's numbers.
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
	known     bool      // we have seen at least one response's headers
	probing   bool      // the one request sent to learn the budget is in flight
	limit     int       // x-ratelimit-limit
	remaining int       // x-ratelimit-remaining, lowest seen this window
	reset     time.Time // x-ratelimit-reset; zero after we roll into a window we haven't seen yet
	reserved  int       // admitted requests not yet answered
	waiting   []*waiter // held requests, oldest first
	timerAt   time.Time // when the pending wake-up timer fires, to avoid duplicates

	spent  []spend   // points of requests sent within the last PointsWindow, oldest first
	paused time.Time // no request goes out before this (after a secondary-limit answer)
	streak int       // secondary-limit answers in a row without retry-after
}

type spend struct {
	at   time.Time
	cost int
}

type waiter struct {
	cost     int
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
	return &Budget{cfg: cfg, now: now, schedule: schedule}
}

// AfterFunc adapts time.AfterFunc to New's schedule parameter.
func AfterFunc(d time.Duration, f func()) { time.AfterFunc(d, f) }

// Acquire blocks until the request may be sent, ctx is done, or the wait
// would exceed MaxHold (a *TooLongError). write says whether the request is
// a write, which costs more points. On success the caller must call exactly
// one of the Ticket's methods when the upstream answers or fails.
func (b *Budget) Acquire(ctx context.Context, write bool) (*Ticket, error) {
	cost := ReadCost
	if write {
		cost = WriteCost
	}
	b.mu.Lock()
	if len(b.waiting) == 0 && b.admitLocked(cost) {
		b.mu.Unlock()
		return &Ticket{b: b}, nil
	}
	now := b.now()
	if err := b.tooLongLocked(now, now.Add(b.cfg.MaxHold)); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	w := &waiter{cost: cost, deadline: now.Add(b.cfg.MaxHold), done: make(chan error, 1)}
	b.waiting = append(b.waiting, w)
	b.scheduleLocked(now)
	b.schedule(b.cfg.MaxHold, b.drain)
	b.mu.Unlock()

	select {
	case err := <-w.done:
		if err != nil {
			return nil, err
		}
		return &Ticket{b: b}, nil
	case <-ctx.Done():
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, x := range b.waiting {
			if x == w {
				b.waiting = append(b.waiting[:i], b.waiting[i+1:]...)
				return nil, ctx.Err()
			}
		}
		// drain already answered w just as ctx ended. If it admitted w,
		// give the unit back so nothing is reserved for a request never sent.
		if err := <-w.done; err == nil {
			b.reserved--
			// Its points stay counted: a little conservative, never unsafe.
			if !b.known {
				b.probing = false
			}
			b.drainLocked()
		}
		return nil, ctx.Err()
	}
}

// admitLocked reserves one unit, one in-flight slot and cost points if all
// four checks pass now.
func (b *Budget) admitLocked(cost int) bool {
	now := b.now()
	b.rollLocked()
	b.pruneLocked(now)
	if now.Before(b.paused) || b.reserved >= b.cfg.MaxInFlight || b.pointsLocked()+cost > b.cfg.Points {
		return false
	}
	switch {
	case !b.known && !b.probing:
		// Learn first: one request goes out; the rest wait for its headers.
		b.probing = true
	case !b.known:
		return false
	case b.primaryOutLocked():
		return false
	}
	b.reserved++
	b.spent = append(b.spent, spend{now, cost})
	return true
}

// primaryOutLocked reports whether the primary budget is used up until the
// reset: every remaining unit is already reserved.
func (b *Budget) primaryOutLocked() bool {
	return b.known && b.remaining-b.reserved <= 0
}

// pruneLocked forgets points spent more than PointsWindow ago.
func (b *Budget) pruneLocked(now time.Time) {
	i := 0
	for i < len(b.spent) && !b.spent[i].at.Add(b.cfg.PointsWindow).After(now) {
		i++
	}
	b.spent = b.spent[i:]
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
func (b *Budget) rollLocked() {
	if b.known && !b.reset.IsZero() && !b.now().Before(b.reset.Add(b.cfg.ResetGuard)) {
		b.remaining = b.limit
		b.reset = time.Time{}
	}
}

// tooLongLocked reports whether a request that can't be sent now would
// wait past deadline. It refuses early only when a known wait (the primary
// reset, or a pause) already runs past the deadline; waits for in-flight
// answers or for points to expire are short, so those requests stay until
// their deadline.
func (b *Budget) tooLongLocked(now, deadline time.Time) error {
	release := b.paused
	if b.primaryOutLocked() && !b.reset.IsZero() {
		release = laterOf(release, b.reset.Add(b.cfg.ResetGuard))
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
// moment a check can start passing on its own: the primary reset, the end
// of a pause, or the oldest points leaving the window. (In-flight answers
// wake the line themselves.)
func (b *Budget) scheduleLocked(now time.Time) {
	var at time.Time
	consider := func(t time.Time) {
		if t.After(now) && (at.IsZero() || t.Before(at)) {
			at = t
		}
	}
	if b.known && !b.reset.IsZero() {
		consider(b.reset.Add(b.cfg.ResetGuard))
	}
	consider(b.paused)
	if len(b.spent) > 0 {
		consider(b.spent[0].at.Add(b.cfg.PointsWindow))
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

// drainLocked admits held requests in arrival order while budget allows,
// and turns away any that can no longer be served within their max hold.
func (b *Budget) drainLocked() {
	now := b.now()
	admitting := true
	kept := b.waiting[:0]
	for _, w := range b.waiting {
		if admitting && b.admitLocked(w.cost) {
			w.done <- nil
			continue
		}
		admitting = false // keep arrival order: nobody jumps the line
		if err := b.tooLongLocked(now, w.deadline); err != nil {
			w.done <- err
			continue
		}
		kept = append(kept, w)
	}
	clear(b.waiting[len(kept):])
	b.waiting = kept
	if len(b.waiting) > 0 {
		b.scheduleLocked(now)
	}
}

// Ticket is one admitted request's reservation.
type Ticket struct {
	b    *Budget
	once sync.Once
}

// Done corrects the budget from the upstream's response and releases the
// reservation. A secondary-limit answer (a 429, or a 403 while primary
// budget remains) pauses every request on the token. A response without
// rate-limit headers counts as Failed, apart from any pause it asks for.
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
		b.reserved--
		b.probing = false
		b.known = true
		b.limit = limit
		if status < 300 {
			b.streak = 0
		}
		switch r := time.Unix(reset, 0); {
		case b.reset.IsZero() || r.After(b.reset):
			// A window we haven't seen yet: take its count as is.
			b.reset = r
			b.remaining = remaining
		case r.Equal(b.reset):
			b.remaining = min(b.remaining, remaining)
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
		b.reserved--
		if b.known {
			b.remaining--
		} else {
			b.probing = false // let the next request try to learn the budget
		}
		b.drainLocked()
	})
}

// secondary pauses the token after a secondary-limit answer, following
// GitHub's order: use retry-after if given; if primary budget is at 0, the
// primary rules already hold requests until the reset; otherwise wait
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
