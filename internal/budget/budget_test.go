package budget

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeTime is a hand-driven clock with a scheduler that runs due funcs
// when the test advances it.
type fakeTime struct {
	mu    sync.Mutex
	t     time.Time
	timer []timer
}

type timer struct {
	at time.Time
	f  func()
}

func (c *fakeTime) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeTime) schedule(d time.Duration, f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timer = append(c.timer, timer{c.t.Add(d), f})
}

// advance moves the clock and runs every func that came due, in time order.
func (c *fakeTime) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	sort.Slice(c.timer, func(i, j int) bool { return c.timer[i].at.Before(c.timer[j].at) })
	var due []func()
	kept := c.timer[:0]
	for _, tm := range c.timer {
		if !tm.at.After(c.t) {
			due = append(due, tm.f)
		} else {
			kept = append(kept, tm)
		}
	}
	c.timer = kept
	c.mu.Unlock()
	for _, f := range due {
		f()
	}
}

var t0 = time.Unix(1_800_000_000, 0)

// core returns the budget's core resource, the one most tests use.
func core(b *Budget) *resource { return b.resources["core"] }

func newTest(maxHold time.Duration) (*Budget, *fakeTime) {
	return newTestWith(Config{MaxHold: maxHold})
}

// newTestWith fills in roomy secondary limits for any left at zero (rather
// than GitHub's), so a test only sets the limits it is about.
func newTestWith(cfg Config) (*Budget, *fakeTime) {
	c := &fakeTime{t: t0}
	if cfg.MaxInFlight == 0 {
		cfg.MaxInFlight = 1000
	}
	if cfg.Points == 0 {
		cfg.Points = 1_000_000
	}
	if cfg.PointsWindow == 0 {
		cfg.PointsWindow = time.Minute
	}
	if cfg.MinPause == 0 {
		cfg.MinPause = time.Minute
	}
	return New(cfg, c.now, c.schedule), c
}

func headers(limit, remaining int, reset time.Time) http.Header {
	h := http.Header{}
	h.Set("x-ratelimit-limit", strconv.Itoa(limit))
	h.Set("x-ratelimit-remaining", strconv.Itoa(remaining))
	h.Set("x-ratelimit-reset", strconv.FormatInt(reset.Unix(), 10))
	return h
}

// acquireAsync starts Acquire in a goroutine and returns its result channel.
func acquireAsync(ctx context.Context, b *Budget) chan result {
	ch := make(chan result, 1)
	go func() {
		t, err := b.Acquire(ctx, "core", false)
		ch <- result{t, err}
	}()
	return ch
}

type result struct {
	t   *Ticket
	err error
}

// waitingCount polls until n requests are held, so tests don't race the
// goroutines they started.
func waitingCount(t *testing.T, b *Budget, n int) {
	t.Helper()
	for range 1000 {
		b.mu.Lock()
		got := 0
		for _, r := range b.resources {
			got += len(r.waiting)
		}
		b.mu.Unlock()
		if got == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("never saw %d held requests", n)
}

func mustGet(t *testing.T, ch chan result) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(time.Second):
		t.Fatal("request still held")
	}
	return result{}
}

func mustBeHeld(t *testing.T, ch chan result) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("request should be held, got %+v", r)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestLearnsFirst(t *testing.T) {
	b, _ := newTest(30 * time.Second)
	first, err := b.Acquire(context.Background(), "core", false)
	if err != nil {
		t.Fatal(err)
	}
	second := acquireAsync(context.Background(), b)
	waitingCount(t, b, 1)
	mustBeHeld(t, second)

	first.Done(200, headers(5, 4, t0.Add(time.Minute)))
	if r := mustGet(t, second); r.err != nil {
		t.Fatalf("second request: %v", r.err)
	}
}

func TestHoldsWhenReservationsUseUpRemaining(t *testing.T) {
	b, _ := newTest(30 * time.Second)
	probe, _ := b.Acquire(context.Background(), "core", false)
	probe.Done(200, headers(10, 3, t0.Add(10*time.Second)))

	for i := range 3 {
		if _, err := b.Acquire(context.Background(), "core", false); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	fourth := acquireAsync(context.Background(), b)
	waitingCount(t, b, 1)
	mustBeHeld(t, fourth)
}

func TestStaleResponseCantRaiseCount(t *testing.T) {
	b, _ := newTest(30 * time.Second)
	reset := t0.Add(10 * time.Second)
	probe, _ := b.Acquire(context.Background(), "core", false)
	probe.Done(200, headers(10, 5, reset))

	a, _ := b.Acquire(context.Background(), "core", false)
	c, _ := b.Acquire(context.Background(), "core", false)
	c.Done(200, headers(10, 2, reset)) // the newer answer arrives first
	a.Done(200, headers(10, 4, reset)) // the older one arrives late
	if core(b).remaining != 2 {
		t.Fatalf("remaining = %d, want 2 (the lowest seen this window)", core(b).remaining)
	}

	// A response from a new window replaces the count.
	d, _ := b.Acquire(context.Background(), "core", false)
	d.Done(200, headers(10, 9, reset.Add(time.Minute)))
	if core(b).remaining != 9 {
		t.Fatalf("remaining = %d, want 9 (new window)", core(b).remaining)
	}
}

func TestResetReleasesHeldInOrderUpToLimit(t *testing.T) {
	b, c := newTest(30 * time.Second)
	probe, _ := b.Acquire(context.Background(), "core", false)
	probe.Done(200, headers(2, 0, t0.Add(10*time.Second)))

	var held []chan result
	for i := range 3 {
		held = append(held, acquireAsync(context.Background(), b))
		waitingCount(t, b, i+1)
	}
	c.advance(10 * time.Second)

	// The limit is 2: the first two held requests go, the third waits on.
	for i := range 2 {
		if r := mustGet(t, held[i]); r.err != nil {
			t.Fatalf("held request %d: %v", i, r.err)
		}
	}
	mustBeHeld(t, held[2])
}

func TestTooLongIsTurnedAwayAtOnce(t *testing.T) {
	b, c := newTest(5 * time.Second)
	probe, _ := b.Acquire(context.Background(), "core", false)
	probe.Done(200, headers(10, 0, t0.Add(42*time.Second)))

	c.advance(300 * time.Millisecond) // 41.7s to go rounds up to 42
	_, err := b.Acquire(context.Background(), "core", false)
	var tl *TooLongError
	if !errors.As(err, &tl) || tl.RetryAfter != 42 {
		t.Fatalf("err = %v, want TooLongError with RetryAfter 42", err)
	}
	if !errors.Is(err, ErrTooLong) {
		t.Fatal("errors.Is(err, ErrTooLong) should be true")
	}
}

func TestHeldPastMaxHoldIsTurnedAway(t *testing.T) {
	b, c := newTest(5 * time.Second)
	probe, _ := b.Acquire(context.Background(), "core", false) // the probe never answers
	held := acquireAsync(context.Background(), b)
	waitingCount(t, b, 1)

	c.advance(5 * time.Second)
	if r := mustGet(t, held); !errors.Is(r.err, ErrTooLong) {
		t.Fatalf("err = %v, want ErrTooLong after max hold", r.err)
	}
	_ = probe
}

func TestClientGivingUpLeavesTheLine(t *testing.T) {
	b, _ := newTest(30 * time.Second)
	probe, _ := b.Acquire(context.Background(), "core", false)
	probe.Done(200, headers(10, 0, t0.Add(10*time.Second)))

	ctx, cancel := context.WithCancel(context.Background())
	held := acquireAsync(ctx, b)
	waitingCount(t, b, 1)
	cancel()
	if r := mustGet(t, held); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", r.err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(core(b).waiting) != 0 || core(b).reserved != 0 || b.inFlight != 0 {
		t.Fatalf("waiting %d reserved %d in flight %d, want 0, 0 and 0", len(core(b).waiting), core(b).reserved, b.inFlight)
	}
}

func TestFailedRequestStaysSpent(t *testing.T) {
	b, _ := newTest(30 * time.Second)
	probe, _ := b.Acquire(context.Background(), "core", false)
	probe.Done(200, headers(10, 1, t0.Add(10*time.Second)))

	tk, _ := b.Acquire(context.Background(), "core", false)
	tk.Failed()
	if core(b).remaining != 0 || core(b).reserved != 0 {
		t.Fatalf("remaining %d reserved %d, want 0 and 0", core(b).remaining, core(b).reserved)
	}
}

func TestFailedProbeLetsNextRequestLearn(t *testing.T) {
	b, _ := newTest(30 * time.Second)
	probe, _ := b.Acquire(context.Background(), "core", false)
	held := acquireAsync(context.Background(), b)
	waitingCount(t, b, 1)
	probe.Failed()
	if r := mustGet(t, held); r.err != nil {
		t.Fatalf("next request should become the probe: %v", r.err)
	}
}

func TestResponseWithoutHeadersCountsAsFailed(t *testing.T) {
	b, _ := newTest(30 * time.Second)
	probe, _ := b.Acquire(context.Background(), "core", false)
	probe.Done(200, headers(10, 2, t0.Add(10*time.Second)))
	tk, _ := b.Acquire(context.Background(), "core", false)
	tk.Done(200, http.Header{})
	tk.Done(200, headers(10, 9, t0.Add(10*time.Second))) // a second call is ignored
	if core(b).remaining != 1 || core(b).reserved != 0 {
		t.Fatalf("remaining %d reserved %d, want 1 and 0", core(b).remaining, core(b).reserved)
	}
}

// learned returns a budget that has already seen a response with plenty of
// primary budget, so tests below exercise only the secondary limits.
func learned(t *testing.T, cfg Config) (*Budget, *fakeTime) {
	t.Helper()
	b, c := newTestWith(cfg)
	probe, err := b.Acquire(context.Background(), "core", false)
	if err != nil {
		t.Fatal(err)
	}
	probe.Done(200, headers(5000, 4999, t0.Add(time.Hour)))
	return b, c
}

func acquireWrite(ctx context.Context, b *Budget, write bool) chan result {
	ch := make(chan result, 1)
	go func() {
		t, err := b.Acquire(ctx, "core", write)
		ch <- result{t, err}
	}()
	return ch
}

func TestInFlightCap(t *testing.T) {
	b, _ := learned(t, Config{MaxHold: 30 * time.Second, MaxInFlight: 2})
	first, _ := b.Acquire(context.Background(), "core", false)
	if _, err := b.Acquire(context.Background(), "core", false); err != nil {
		t.Fatal(err)
	}
	third := acquireAsync(context.Background(), b)
	waitingCount(t, b, 1)
	mustBeHeld(t, third)

	first.Done(200, headers(5000, 4997, t0.Add(time.Hour)))
	if r := mustGet(t, third); r.err != nil {
		t.Fatalf("third request after an answer: %v", r.err)
	}
}

func TestPointsCap(t *testing.T) {
	// The probe in learned() costs 1 point, so allow 8: the probe, then a
	// write (5) and two reads (1 each) fit, and a third read doesn't.
	b, c := learned(t, Config{MaxHold: 2 * time.Minute, Points: 8, PointsWindow: time.Minute})
	for i, write := range []bool{true, false, false} {
		if _, err := b.Acquire(context.Background(), "core", write); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	fourth := acquireWrite(context.Background(), b, false)
	waitingCount(t, b, 1)
	mustBeHeld(t, fourth)

	// Points older than a minute stop counting.
	c.advance(time.Minute)
	if r := mustGet(t, fourth); r.err != nil {
		t.Fatalf("read after the window slid: %v", r.err)
	}
}

func TestPointsCountUntilAWindowAfterTheAnswer(t *testing.T) {
	b, c := learned(t, Config{MaxHold: 5 * time.Minute, Points: 6, PointsWindow: time.Minute})
	c.advance(40 * time.Second)
	w, err := b.Acquire(context.Background(), "core", true) // 1 (probe) + 5 = 6 points
	if err != nil {
		t.Fatal(err)
	}
	held := acquireWrite(context.Background(), b, true)
	waitingCount(t, b, 1)

	// While the write is in flight, its points never expire.
	c.advance(2 * time.Minute)
	mustBeHeld(t, held)

	// Once answered, they count for one more window, then the next write fits.
	w.Done(200, headers(5000, 4998, t0.Add(time.Hour)))
	c.advance(59 * time.Second)
	mustBeHeld(t, held)
	c.advance(time.Second)
	if r := mustGet(t, held); r.err != nil {
		t.Fatalf("write a window after the first write's answer: %v", r.err)
	}
}

func secondaryHeaders(retryAfter string) http.Header {
	h := headers(5000, 4000, t0.Add(time.Hour))
	if retryAfter != "" {
		h.Set("retry-after", retryAfter)
	}
	return h
}

func TestRetryAfterPausesEveryone(t *testing.T) {
	b, c := learned(t, Config{MaxHold: 30 * time.Second})
	tk, _ := b.Acquire(context.Background(), "core", false)
	tk.Done(http.StatusTooManyRequests, secondaryHeaders("3"))

	held := acquireAsync(context.Background(), b)
	waitingCount(t, b, 1)
	c.advance(2 * time.Second)
	mustBeHeld(t, held)
	c.advance(time.Second)
	if r := mustGet(t, held); r.err != nil {
		t.Fatalf("after retry-after: %v", r.err)
	}
}

func TestPauseWithoutRetryAfterDoublesAndResets(t *testing.T) {
	b, c := learned(t, Config{MaxHold: 10 * time.Minute, MinPause: time.Minute})

	pauseFor := func(status int) time.Duration {
		t.Helper()
		tk, err := b.Acquire(context.Background(), "core", false)
		if err != nil {
			t.Fatal(err)
		}
		tk.Done(status, secondaryHeaders(""))
		b.mu.Lock()
		d := b.paused.Sub(c.now())
		b.mu.Unlock()
		c.advance(max(d, 0))
		return d
	}

	if d := pauseFor(http.StatusTooManyRequests); d != time.Minute {
		t.Fatalf("first pause %v, want 1m", d)
	}
	if d := pauseFor(http.StatusForbidden); d != 2*time.Minute { // a secondary 403 counts too
		t.Fatalf("second pause %v, want 2m", d)
	}
	// A success resets the doubling.
	if d := pauseFor(http.StatusOK); d > 0 {
		t.Fatalf("a 200 paused for %v", d)
	}
	if d := pauseFor(http.StatusTooManyRequests); d != time.Minute {
		t.Fatalf("pause after a success %v, want 1m", d)
	}
}

func TestPauseLongerThanMaxHoldIsTurnedAway(t *testing.T) {
	b, _ := learned(t, Config{MaxHold: 5 * time.Second})
	tk, _ := b.Acquire(context.Background(), "core", false)
	tk.Done(http.StatusTooManyRequests, secondaryHeaders("30"))

	_, err := b.Acquire(context.Background(), "core", false)
	var tl *TooLongError
	if !errors.As(err, &tl) || tl.RetryAfter != 30 {
		t.Fatalf("err = %v, want TooLongError with RetryAfter 30", err)
	}
}

func TestLineKeepsArrivalOrderAcrossChecks(t *testing.T) {
	// One slot in flight. A write arrives first and a read second; when the
	// slot frees, the write must go first even though points allow both.
	b, _ := learned(t, Config{MaxHold: 30 * time.Second, MaxInFlight: 1})
	busy, _ := b.Acquire(context.Background(), "core", false)
	write := acquireWrite(context.Background(), b, true)
	waitingCount(t, b, 1)
	read := acquireWrite(context.Background(), b, false)
	waitingCount(t, b, 2)

	busy.Done(200, headers(5000, 4997, t0.Add(time.Hour)))
	w := mustGet(t, write)
	if w.err != nil {
		t.Fatalf("write: %v", w.err)
	}
	mustBeHeld(t, read)
	w.t.Done(200, headers(5000, 4996, t0.Add(time.Hour)))
	if r := mustGet(t, read); r.err != nil {
		t.Fatalf("read: %v", r.err)
	}
}

// Per-resource budgets: GitHub counts core, search, ... separately.

func resHeaders(res string, limit, remaining int, reset time.Time) http.Header {
	h := headers(limit, remaining, reset)
	h.Set("x-ratelimit-resource", res)
	return h
}

func acquireRes(ctx context.Context, b *Budget, res string, write bool) chan result {
	ch := make(chan result, 1)
	go func() {
		t, err := b.Acquire(ctx, res, write)
		ch <- result{t, err}
	}()
	return ch
}

// mixed returns a budget that knows core (plenty left) and search.
func mixed(t *testing.T, cfg Config, searchLeft int) (*Budget, *fakeTime) {
	t.Helper()
	b, c := learned(t, cfg)
	tk, err := b.Acquire(context.Background(), "search", false)
	if err != nil {
		t.Fatal(err)
	}
	tk.Done(200, resHeaders("search", 30, searchLeft, t0.Add(time.Minute)))
	return b, c
}

func TestSearchAtZeroIsHeldAfterCore(t *testing.T) {
	// Before per-resource budgets, core's later reset hid search's 0.
	b, c := mixed(t, Config{MaxHold: 2 * time.Minute}, 0)
	held := acquireRes(context.Background(), b, "search", false)
	waitingCount(t, b, 1)
	mustBeHeld(t, held)
	c.advance(time.Minute)
	if r := mustGet(t, held); r.err != nil {
		t.Fatalf("search after its reset: %v", r.err)
	}
}

func TestCoreGoesWhileSearchIsAtZero(t *testing.T) {
	b, _ := mixed(t, Config{MaxHold: 2 * time.Minute}, 0)
	search := acquireRes(context.Background(), b, "search", false)
	waitingCount(t, b, 1)
	// A core call arrives after the held search and goes at once.
	if _, err := b.Acquire(context.Background(), "core", false); err != nil {
		t.Fatalf("core call: %v", err)
	}
	mustBeHeld(t, search)
}

func TestEachResourceLearnsOnItsOwn(t *testing.T) {
	b, _ := learned(t, Config{MaxHold: 30 * time.Second})
	probe, err := b.Acquire(context.Background(), "search", false)
	if err != nil {
		t.Fatal(err)
	}
	second := acquireRes(context.Background(), b, "search", false)
	waitingCount(t, b, 1)
	mustBeHeld(t, second)
	if _, err := b.Acquire(context.Background(), "core", false); err != nil {
		t.Fatalf("core is known and should go: %v", err)
	}
	probe.Done(200, resHeaders("search", 30, 29, t0.Add(time.Minute)))
	if r := mustGet(t, second); r.err != nil {
		t.Fatalf("second search after the probe: %v", r.err)
	}
}

func TestInFlightCapIsSharedAcrossResources(t *testing.T) {
	b, _ := mixed(t, Config{MaxHold: 30 * time.Second, MaxInFlight: 2}, 29)
	first, _ := b.Acquire(context.Background(), "core", false)
	if _, err := b.Acquire(context.Background(), "search", false); err != nil {
		t.Fatal(err)
	}
	third := acquireRes(context.Background(), b, "core", false)
	waitingCount(t, b, 1)
	mustBeHeld(t, third)
	first.Done(200, resHeaders("core", 5000, 4990, t0.Add(time.Hour)))
	if r := mustGet(t, third); r.err != nil {
		t.Fatalf("third call after an answer: %v", r.err)
	}
}

func TestSecondaryPauseOnSearchPausesCore(t *testing.T) {
	b, c := mixed(t, Config{MaxHold: 30 * time.Second}, 29)
	tk, _ := b.Acquire(context.Background(), "search", false)
	h := resHeaders("search", 30, 28, t0.Add(time.Minute))
	h.Set("retry-after", "3")
	tk.Done(http.StatusTooManyRequests, h)

	held := acquireRes(context.Background(), b, "core", false)
	waitingCount(t, b, 1)
	c.advance(2 * time.Second)
	mustBeHeld(t, held)
	c.advance(time.Second)
	if r := mustGet(t, held); r.err != nil {
		t.Fatalf("core after the pause: %v", r.err)
	}
}

func TestOldestThatCanGoGoesFirstAcrossLines(t *testing.T) {
	// One slot in flight. In line: a search (search at 0), then core A,
	// then core B. When the slot frees, core A goes: the search can't, and
	// A is older than B.
	b, c := mixed(t, Config{MaxHold: 2 * time.Minute, MaxInFlight: 1}, 0)
	busy, _ := b.Acquire(context.Background(), "core", false)
	search := acquireRes(context.Background(), b, "search", false)
	waitingCount(t, b, 1)
	a := acquireRes(context.Background(), b, "core", false)
	waitingCount(t, b, 2)
	bb := acquireRes(context.Background(), b, "core", false)
	waitingCount(t, b, 3)

	busy.Done(200, resHeaders("core", 5000, 4990, t0.Add(time.Hour)))
	ra := mustGet(t, a)
	if ra.err != nil {
		t.Fatalf("core A: %v", ra.err)
	}
	mustBeHeld(t, bb)
	mustBeHeld(t, search)

	// Core B goes next. At search's reset the search still waits for the
	// slot, then goes when B answers.
	ra.t.Done(200, resHeaders("core", 5000, 4989, t0.Add(time.Hour)))
	r := mustGet(t, bb)
	if r.err != nil {
		t.Fatalf("core B: %v", r.err)
	}
	c.advance(time.Minute)
	mustBeHeld(t, search) // the slot is B's
	r.t.Done(200, resHeaders("core", 5000, 4988, t0.Add(time.Hour)))
	if r := mustGet(t, search); r.err != nil {
		t.Fatalf("search after its reset: %v", r.err)
	}
}

func TestAnswerCorrectsTheResourceItNames(t *testing.T) {
	b, _ := learned(t, Config{MaxHold: 30 * time.Second})
	tk, _ := b.Acquire(context.Background(), "core", false) // guessed core
	tk.Done(200, resHeaders("dependency_snapshots", 100, 0, t0.Add(time.Hour)))
	b.mu.Lock()
	defer b.mu.Unlock()
	if core(b).remaining != 4999 || core(b).reserved != 0 {
		t.Fatalf("core remaining %d reserved %d, want 4999 and 0", core(b).remaining, core(b).reserved)
	}
	if d := b.resources["dependency_snapshots"]; d == nil || !d.known || d.remaining != 0 {
		t.Fatalf("dependency_snapshots = %+v, want known with 0 left", d)
	}
}
