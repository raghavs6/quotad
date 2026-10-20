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
	c := &fakeTime{t: t0}
	return New(Config{MaxHold: maxHold, ResetGuard: 0}, c.now, c.schedule), c
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
		t, err := b.Acquire(ctx)
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
	first, err := b.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second := acquireAsync(context.Background(), b)
	waitingCount(t, b, 1)
	mustBeHeld(t, second)

	first.Done(headers(5, 4, t0.Add(time.Minute)))
	if r := mustGet(t, second); r.err != nil {
		t.Fatalf("second request: %v", r.err)
	}
}

func TestHoldsWhenReservationsUseUpRemaining(t *testing.T) {
	b, _ := newTest(30 * time.Second)
	probe, _ := b.Acquire(context.Background())
	probe.Done(headers(10, 3, t0.Add(10*time.Second)))

	for i := range 3 {
		if _, err := b.Acquire(context.Background()); err != nil {
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
	probe, _ := b.Acquire(context.Background())
	probe.Done(headers(10, 5, reset))

	a, _ := b.Acquire(context.Background())
	c, _ := b.Acquire(context.Background())
	c.Done(headers(10, 2, reset)) // the newer answer arrives first
	a.Done(headers(10, 4, reset)) // the older one arrives late
	if b.remaining != 2 {
		t.Fatalf("remaining = %d, want 2 (the lowest seen this window)", b.remaining)
	}

	// A response from a new window replaces the count.
	d, _ := b.Acquire(context.Background())
	d.Done(headers(10, 9, reset.Add(time.Minute)))
	if b.remaining != 9 {
		t.Fatalf("remaining = %d, want 9 (new window)", b.remaining)
	}
}

func TestResetReleasesHeldInOrderUpToLimit(t *testing.T) {
	b, c := newTest(30 * time.Second)
	probe, _ := b.Acquire(context.Background())
	probe.Done(headers(2, 0, t0.Add(10*time.Second)))

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
	probe, _ := b.Acquire(context.Background())
	probe.Done(headers(10, 0, t0.Add(42*time.Second)))

	c.advance(300 * time.Millisecond) // 41.7s to go rounds up to 42
	_, err := b.Acquire(context.Background())
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
	probe, _ := b.Acquire(context.Background()) // the probe never answers
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
	probe, _ := b.Acquire(context.Background())
	probe.Done(headers(10, 0, t0.Add(10*time.Second)))

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
	probe, _ := b.Acquire(context.Background())
	probe.Done(headers(10, 1, t0.Add(10*time.Second)))

	tk, _ := b.Acquire(context.Background())
	tk.Failed()
	if b.remaining != 0 || b.reserved != 0 {
		t.Fatalf("remaining %d reserved %d, want 0 and 0", b.remaining, b.reserved)
	}
}

func TestFailedProbeLetsNextRequestLearn(t *testing.T) {
	b, _ := newTest(30 * time.Second)
	probe, _ := b.Acquire(context.Background())
	held := acquireAsync(context.Background(), b)
	waitingCount(t, b, 1)
	probe.Failed()
	if r := mustGet(t, held); r.err != nil {
		t.Fatalf("next request should become the probe: %v", r.err)
	}
}

func TestResponseWithoutHeadersCountsAsFailed(t *testing.T) {
	b, _ := newTest(30 * time.Second)
	probe, _ := b.Acquire(context.Background())
	probe.Done(headers(10, 2, t0.Add(10*time.Second)))
	tk, _ := b.Acquire(context.Background())
	tk.Done(http.Header{})
	tk.Done(headers(10, 9, t0.Add(10*time.Second))) // a second call is ignored
	if b.remaining != 1 || b.reserved != 0 {
		t.Fatalf("remaining %d reserved %d, want 1 and 0", b.remaining, b.reserved)
	}
}
