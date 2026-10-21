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
		t, err := b.Acquire(ctx, false)
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
		got := len(b.waiting)
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
	first, err := b.Acquire(context.Background(), false)
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
	probe, _ := b.Acquire(context.Background(), false)
	probe.Done(200, headers(10, 3, t0.Add(10*time.Second)))

	for i := range 3 {
		if _, err := b.Acquire(context.Background(), false); err != nil {
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
	probe, _ := b.Acquire(context.Background(), false)
	probe.Done(200, headers(10, 5, reset))

	a, _ := b.Acquire(context.Background(), false)
	c, _ := b.Acquire(context.Background(), false)
	c.Done(200, headers(10, 2, reset)) // the newer answer arrives first
	a.Done(200, headers(10, 4, reset)) // the older one arrives late
	if b.remaining != 2 {
		t.Fatalf("remaining = %d, want 2 (the lowest seen this window)", b.remaining)
	}

	// A response from a new window replaces the count.
	d, _ := b.Acquire(context.Background(), false)
	d.Done(200, headers(10, 9, reset.Add(time.Minute)))
	if b.remaining != 9 {
		t.Fatalf("remaining = %d, want 9 (new window)", b.remaining)
	}
}

func TestResetReleasesHeldInOrderUpToLimit(t *testing.T) {
	b, c := newTest(30 * time.Second)
	probe, _ := b.Acquire(context.Background(), false)
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
	probe, _ := b.Acquire(context.Background(), false)
	probe.Done(200, headers(10, 0, t0.Add(42*time.Second)))

	c.advance(300 * time.Millisecond) // 41.7s to go rounds up to 42
	_, err := b.Acquire(context.Background(), false)
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
	probe, _ := b.Acquire(context.Background(), false) // the probe never answers
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
	probe, _ := b.Acquire(context.Background(), false)
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
	if len(b.waiting) != 0 || b.reserved != 0 {
		t.Fatalf("waiting %d reserved %d, want 0 and 0", len(b.waiting), b.reserved)
	}
}

func TestFailedRequestStaysSpent(t *testing.T) {
	b, _ := newTest(30 * time.Second)
	probe, _ := b.Acquire(context.Background(), false)
	probe.Done(200, headers(10, 1, t0.Add(10*time.Second)))

	tk, _ := b.Acquire(context.Background(), false)
	tk.Failed()
	if b.remaining != 0 || b.reserved != 0 {
		t.Fatalf("remaining %d reserved %d, want 0 and 0", b.remaining, b.reserved)
	}
}

func TestFailedProbeLetsNextRequestLearn(t *testing.T) {
	b, _ := newTest(30 * time.Second)
	probe, _ := b.Acquire(context.Background(), false)
	held := acquireAsync(context.Background(), b)
	waitingCount(t, b, 1)
	probe.Failed()
	if r := mustGet(t, held); r.err != nil {
		t.Fatalf("next request should become the probe: %v", r.err)
	}
}

func TestResponseWithoutHeadersCountsAsFailed(t *testing.T) {
	b, _ := newTest(30 * time.Second)
	probe, _ := b.Acquire(context.Background(), false)
	probe.Done(200, headers(10, 2, t0.Add(10*time.Second)))
	tk, _ := b.Acquire(context.Background(), false)
	tk.Done(200, http.Header{})
	tk.Done(200, headers(10, 9, t0.Add(10*time.Second))) // a second call is ignored
	if b.remaining != 1 || b.reserved != 0 {
		t.Fatalf("remaining %d reserved %d, want 1 and 0", b.remaining, b.reserved)
	}
}

// learned returns a budget that has already seen a response with plenty of
// primary budget, so tests below exercise only the secondary limits.
func learned(t *testing.T, cfg Config) (*Budget, *fakeTime) {
	t.Helper()
	b, c := newTestWith(cfg)
	probe, err := b.Acquire(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	probe.Done(200, headers(5000, 4999, t0.Add(time.Hour)))
	return b, c
}

func acquireWrite(ctx context.Context, b *Budget, write bool) chan result {
	ch := make(chan result, 1)
	go func() {
		t, err := b.Acquire(ctx, write)
		ch <- result{t, err}
	}()
	return ch
}

func TestInFlightCap(t *testing.T) {
	b, _ := learned(t, Config{MaxHold: 30 * time.Second, MaxInFlight: 2})
	first, _ := b.Acquire(context.Background(), false)
	if _, err := b.Acquire(context.Background(), false); err != nil {
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
		if _, err := b.Acquire(context.Background(), write); err != nil {
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

func TestPointsWindowSlides(t *testing.T) {
	b, c := learned(t, Config{MaxHold: 2 * time.Minute, Points: 6, PointsWindow: time.Minute})
	c.advance(40 * time.Second)
	if _, err := b.Acquire(context.Background(), true); err != nil { // 1 + 5 = 6 points
		t.Fatal(err)
	}
	// At 60s the probe's point leaves the window, but the write's 5 stay
	// until 100s. A second write must wait for those.
	held := acquireWrite(context.Background(), b, true)
	waitingCount(t, b, 1)
	c.advance(20 * time.Second)
	mustBeHeld(t, held)
	c.advance(40 * time.Second)
	if r := mustGet(t, held); r.err != nil {
		t.Fatalf("write after the first write left the window: %v", r.err)
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
	tk, _ := b.Acquire(context.Background(), false)
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
		tk, err := b.Acquire(context.Background(), false)
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
	tk, _ := b.Acquire(context.Background(), false)
	tk.Done(http.StatusTooManyRequests, secondaryHeaders("30"))

	_, err := b.Acquire(context.Background(), false)
	var tl *TooLongError
	if !errors.As(err, &tl) || tl.RetryAfter != 30 {
		t.Fatalf("err = %v, want TooLongError with RetryAfter 30", err)
	}
}

func TestLineKeepsArrivalOrderAcrossChecks(t *testing.T) {
	// One slot in flight. A write arrives first and a read second; when the
	// slot frees, the write must go first even though points allow both.
	b, _ := learned(t, Config{MaxHold: 30 * time.Second, MaxInFlight: 1})
	busy, _ := b.Acquire(context.Background(), false)
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
