// Package budget tracks one token's primary rate-limit budget, learned from
// response headers, and decides which requests may go to the upstream now,
// which wait, and which are turned away.
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
}

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
	timerAt   time.Time // when the pending reset timer fires, to avoid duplicates
}

type waiter struct {
	deadline time.Time  // arrival + MaxHold
	done     chan error // nil error = admitted
}

// New returns a budget that knows nothing yet. now and schedule are
// injected so tests control time; pass time.Now and AfterFunc for real use.
func New(cfg Config, now func() time.Time, schedule func(time.Duration, func())) *Budget {
	return &Budget{cfg: cfg, now: now, schedule: schedule}
}

// AfterFunc adapts time.AfterFunc to New's schedule parameter.
func AfterFunc(d time.Duration, f func()) { time.AfterFunc(d, f) }

// Acquire blocks until the request may be sent, ctx is done, or the wait
// would exceed MaxHold (a *TooLongError). On success the caller must call
// exactly one of the Ticket's methods when the upstream answers or fails.
func (b *Budget) Acquire(ctx context.Context) (*Ticket, error) {
	b.mu.Lock()
	if len(b.waiting) == 0 && b.admitLocked() {
		b.mu.Unlock()
		return &Ticket{b: b}, nil
	}
	now := b.now()
	if err := b.tooLongLocked(now, now.Add(b.cfg.MaxHold)); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	w := &waiter{deadline: now.Add(b.cfg.MaxHold), done: make(chan error, 1)}
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
			if !b.known {
				b.probing = false
			}
			b.drainLocked()
		}
		return nil, ctx.Err()
	}
}

// admitLocked reserves one unit if the budget allows sending now.
func (b *Budget) admitLocked() bool {
	b.rollLocked()
	switch {
	case !b.known && !b.probing:
		// Learn first: one request goes out; the rest wait for its headers.
		b.probing = true
	case !b.known:
		return false
	case b.remaining-b.reserved <= 0:
		return false
	}
	b.reserved++
	return true
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
// wait past deadline for the next window.
func (b *Budget) tooLongLocked(now, deadline time.Time) error {
	if !b.known || b.reset.IsZero() {
		// Budget will come back when in-flight requests answer, not at a
		// known time. Only a request already past its deadline is refused.
		if !now.Before(deadline) {
			return &TooLongError{RetryAfter: 1}
		}
		return nil
	}
	if release := b.reset.Add(b.cfg.ResetGuard); release.After(deadline) {
		return &TooLongError{RetryAfter: ceilSeconds(release.Sub(now))}
	}
	return nil
}

// scheduleLocked makes sure held requests are looked at again when the
// window resets.
func (b *Budget) scheduleLocked(now time.Time) {
	if !b.known || b.reset.IsZero() {
		return
	}
	at := b.reset.Add(b.cfg.ResetGuard)
	if at.Equal(b.timerAt) {
		return
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
		if admitting && b.admitLocked() {
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

// Done corrects the budget from the upstream's response headers and
// releases the reservation. A response without rate-limit headers counts
// as Failed.
func (t *Ticket) Done(h http.Header) {
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

func ceilSeconds(d time.Duration) int {
	return max(1, int(math.Ceil(d.Seconds())))
}
