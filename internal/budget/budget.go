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
	"cmp"
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
//
// Budget holds requests in lines, one per resource, and asks its counts
// which of them may go. The counts live in this process (New) or in Redis,
// shared by every quotad copy on the token (NewRedis).
type Budget struct {
	cfg      Config
	now      func() time.Time
	schedule func(time.Duration, func()) // runs f after d; time.AfterFunc in production
	c        counts                      // used only with mu held

	mu       sync.Mutex
	lines    map[string][]*waiter // held requests per resource, oldest first
	arrivals uint64               // counts requests as they arrive, to order the lines
	timerAt  time.Time            // when the pending wake-up timer fires, to avoid duplicates
}

type waiter struct {
	arrival  uint64
	res      string
	cost     int
	l        lease      // set when admitted
	deadline time.Time  // arrival + MaxHold
	done     chan error // nil error = admitted
}

// counts keeps one token's limits and decides which requests may go. Budget
// calls its methods with its lock held, one at a time.
type counts interface {
	// admit goes through wants, oldest first, and admits each one that the
	// limits allow. A want whose resource's primary budget is out is skipped,
	// and so is every later want on that resource. The first want that the
	// token's shared limits stop also stops every later want, so nobody
	// jumps ahead of it.
	admit(wants []want) grant
	// answer applies the upstream's answer to l and releases l.
	answer(l lease, a answer)
	// cancel releases l for a request that was never sent.
	cancel(l lease)
}

// want is one held request, as counts sees it.
type want struct {
	res  string
	cost int
}

// lease is one admitted request's reservation, in the form its counts uses.
type lease any

// grant is admit's result.
type grant struct {
	leases []lease // one per want; nil if not admitted
	// release says, for each resource with a want left over, how long until
	// the known part of its wait ends: the primary reset if its budget is
	// out, or the end of a pause. Zero or less means no known wait.
	release map[string]time.Duration
	// wake is when to call admit again for the wants left over, if nothing
	// local happens first. Zero means only a local answer can help.
	wake time.Duration
}

// answer is what the upstream said about one request.
type answer struct {
	status  int
	pause   time.Duration // a secondary-limit answer's retry-after
	backoff bool          // a secondary-limit answer without retry-after
	ok      bool          // the rate-limit headers below were present
	// The headers. resource is x-ratelimit-resource, empty if not sent.
	resource         string
	limit, remaining int
	reset            time.Time
}

// GitHub's published secondary limits, used for any Config field left at zero.
const (
	GitHubMaxInFlight = 100
	GitHubPoints      = 900
	GitHubPointsEvery = time.Minute
	GitHubMinPause    = time.Minute
)

func withDefaults(cfg Config) Config {
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
	return cfg
}

// New returns a budget kept in this process's memory that knows nothing
// yet. Secondary-limit fields left at zero get GitHub's numbers. now and
// schedule are injected so tests control time; pass time.Now and AfterFunc
// for real use.
func New(cfg Config, now func() time.Time, schedule func(time.Duration, func())) *Budget {
	cfg = withDefaults(cfg)
	return newBudget(cfg, now, schedule, newMemory(cfg, now))
}

func newBudget(cfg Config, now func() time.Time, schedule func(time.Duration, func()), c counts) *Budget {
	return &Budget{cfg: cfg, now: now, schedule: schedule, c: c, lines: map[string][]*waiter{}}
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
	b.arrivals++
	w := &waiter{arrival: b.arrivals, res: res, cost: cost, deadline: b.now().Add(b.cfg.MaxHold), done: make(chan error, 1)}
	b.lines[res] = append(b.lines[res], w)
	// Join the back of the line, then let the line move: w goes now only if
	// nobody who came earlier can go first.
	b.drainLocked()
	select {
	case err := <-w.done:
		b.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return &Ticket{b: b, l: w.l}, nil
	default:
	}
	b.schedule(b.cfg.MaxHold, b.drain)
	b.mu.Unlock()

	select {
	case err := <-w.done:
		if err != nil {
			return nil, err
		}
		return &Ticket{b: b, l: w.l}, nil
	case <-ctx.Done():
		b.mu.Lock()
		defer b.mu.Unlock()
		if i := slices.Index(b.lines[res], w); i >= 0 {
			b.lines[res] = slices.Delete(b.lines[res], i, i+1)
			return nil, ctx.Err()
		}
		// drain already answered w just as ctx ended. If it admitted w,
		// give the unit back so nothing is reserved for a request never sent.
		if err := <-w.done; err == nil {
			b.c.cancel(w.l)
			b.drainLocked()
		}
		return nil, ctx.Err()
	}
}

// tooLong reports whether a request that can't be sent now would wait past
// deadline. It refuses early only when a known wait (the primary reset, or
// a pause) already runs past the deadline; waits for in-flight answers or
// for points to expire are short, so those requests stay until their
// deadline.
func tooLong(now, deadline time.Time, release time.Duration) error {
	if (release > 0 && now.Add(release).After(deadline)) || !now.Before(deadline) {
		return &TooLongError{RetryAfter: ceilSeconds(release)}
	}
	return nil
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// scheduleLocked makes sure held requests are looked at again at the
// moment at, unless a timer at or before it is already set.
func (b *Budget) scheduleLocked(now, at time.Time) {
	if !b.timerAt.IsZero() && !at.Before(b.timerAt) {
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

// drainLocked admits held requests while budget allows, and turns away any
// that can no longer be served within their max hold. Each resource's line
// keeps arrival order. Across lines, the oldest head whose resource has
// budget goes next; if the shared checks stop it, nobody behind it jumps
// ahead.
func (b *Budget) drainLocked() {
	// One merged list, oldest first. No more than MaxInFlight per line can
	// go at once, so the rest of a long line needn't be asked about.
	var ws []*waiter
	for _, line := range b.lines {
		ws = append(ws, line[:min(len(line), b.cfg.MaxInFlight)]...)
	}
	if len(ws) == 0 {
		return
	}
	slices.SortFunc(ws, func(x, y *waiter) int { return cmp.Compare(x.arrival, y.arrival) })
	wants := make([]want, len(ws))
	for i, w := range ws {
		wants[i] = want{w.res, w.cost}
	}
	g := b.c.admit(wants)
	for i, w := range ws {
		if g.leases[i] != nil {
			w.l = g.leases[i]
			w.done <- nil
		}
	}

	now := b.now()
	held := false
	for res, line := range b.lines {
		kept := line[:0]
		for _, w := range line {
			if w.l != nil {
				continue
			}
			if err := tooLong(now, w.deadline, g.release[res]); err != nil {
				w.done <- err
				continue
			}
			kept = append(kept, w)
		}
		clear(line[len(kept):])
		if len(kept) == 0 {
			delete(b.lines, res)
			continue
		}
		b.lines[res] = kept
		held = true
	}
	if held && g.wake > 0 {
		b.scheduleLocked(now, now.Add(g.wake))
	}
}

// Ticket is one admitted request's reservation.
type Ticket struct {
	b    *Budget
	l    lease
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
	a := answer{status: status}
	// Secondary limits, in GitHub's order: use retry-after if given; if
	// primary budget is at 0, the primary rules already hold that resource
	// until its reset; otherwise pause MinPause, doubling for each such
	// answer in a row.
	remaining := h.Get("x-ratelimit-remaining")
	if status == http.StatusTooManyRequests || (status == http.StatusForbidden && remaining != "0" && remaining != "") {
		if s, err := strconv.Atoi(h.Get("retry-after")); err == nil && s > 0 {
			a.pause = time.Duration(s) * time.Second
		} else if remaining != "0" {
			a.backoff = true
		}
	}
	limit, err1 := strconv.Atoi(h.Get("x-ratelimit-limit"))
	rem, err2 := strconv.Atoi(remaining)
	reset, err3 := strconv.ParseInt(h.Get("x-ratelimit-reset"), 10, 64)
	if errors.Join(err1, err2, err3) == nil {
		a.ok = true
		a.resource = h.Get("x-ratelimit-resource")
		a.limit, a.remaining, a.reset = limit, rem, time.Unix(reset, 0)
	}
	t.finish(a)
}

// Failed releases the reservation for a request whose upstream gave no
// usable answer (a timeout, a dropped connection). The upstream may still
// have counted it, so the unit stays spent until a response corrects us.
func (t *Ticket) Failed() {
	t.finish(answer{})
}

func (t *Ticket) finish(a answer) {
	t.once.Do(func() {
		b := t.b
		b.mu.Lock()
		defer b.mu.Unlock()
		b.c.answer(t.l, a)
		b.drainLocked()
	})
}

func ceilSeconds(d time.Duration) int {
	return max(1, int(math.Ceil(d.Seconds())))
}
