package fake

import (
	"net/http"
	"testing"
	"time"
)

// clock is a hand-driven clock for tests.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// start is mid-second on purpose, to check windows snap to whole seconds.
var start = time.Date(2026, 10, 19, 12, 0, 0, 400_000_000, time.UTC)

func newTestLimiter(t *testing.T, cfg Config) (*Limiter, *clock) {
	t.Helper()
	c := &clock{t: start}
	l, err := NewLimiter(cfg, c.now)
	if err != nil {
		t.Fatal(err)
	}
	return l, c
}

func roomyConfig() Config {
	return Config{Budget: 5, Window: 60 * time.Second, MaxConcurrent: 100, Points: 1000, PointsWindow: time.Second}
}

// doRead sends one read and releases it right away.
func doRead(l *Limiter) Decision {
	d, release := l.Begin("tok", Core, false)
	if release != nil {
		release()
	}
	return d
}

func TestPrimaryBudgetCountsDown(t *testing.T) {
	l, _ := newTestLimiter(t, roomyConfig())
	for i := 1; i <= 5; i++ {
		d := doRead(l)
		if d.Status != http.StatusOK || d.Remaining != 5-i || d.Used != i || d.Violation {
			t.Fatalf("request %d: got status %d remaining %d used %d violation %v; want 200, %d, %d, false",
				i, d.Status, d.Remaining, d.Used, d.Violation, 5-i, i)
		}
	}
	d := doRead(l)
	if d.Status != http.StatusForbidden || d.Remaining != 0 || !d.Violation {
		t.Fatalf("6th request: got status %d remaining %d violation %v; want 403, 0, true", d.Status, d.Remaining, d.Violation)
	}
}

func TestWindowResetRestoresBudget(t *testing.T) {
	l, c := newTestLimiter(t, roomyConfig())
	first := doRead(l)
	wantReset := time.Date(2026, 10, 19, 12, 1, 0, 0, time.UTC)
	if !first.Reset.Equal(wantReset) {
		t.Fatalf("reset = %v, want %v (window start snapped to a whole second)", first.Reset, wantReset)
	}
	for range 4 {
		doRead(l)
	}
	if d := doRead(l); d.Status != http.StatusForbidden {
		t.Fatalf("before reset: status %d, want 403", d.Status)
	}

	c.t = wantReset
	d := doRead(l)
	if d.Status != http.StatusOK || d.Remaining != 4 || d.Violation {
		t.Fatalf("after reset: status %d remaining %d violation %v; want 200, 4, false", d.Status, d.Remaining, d.Violation)
	}
	if got := l.Stats()["tok"].Windows; got != 2 {
		t.Fatalf("windows = %d, want 2", got)
	}
}

func TestRequestInsideRetryAfterIsViolation(t *testing.T) {
	cfg := roomyConfig()
	cfg.MaxConcurrent = 1
	l, c := newTestLimiter(t, cfg)

	_, release := l.Begin("tok", Core, false) // hold the only slot
	d, _ := l.Begin("tok", Core, false)
	if d.Status != http.StatusTooManyRequests || d.RetryAfter != 1 || d.Violation {
		t.Fatalf("over concurrency: status %d retry-after %d violation %v; want 429, 1, false", d.Status, d.RetryAfter, d.Violation)
	}
	release()

	// Half a second later the slot is free, but we're inside retry-after.
	c.advance(500 * time.Millisecond)
	d = doRead(l)
	if d.Status != http.StatusTooManyRequests || !d.Violation {
		t.Fatalf("inside retry-after: status %d violation %v; want 429, true", d.Status, d.Violation)
	}

	// Once retry-after has passed, the request goes through and isn't counted.
	c.advance(500 * time.Millisecond)
	d = doRead(l)
	if d.Status != http.StatusOK || d.Violation {
		t.Fatalf("after retry-after: status %d violation %v; want 200, false", d.Status, d.Violation)
	}
	if got := l.Stats()["tok"].Violations; got != 1 {
		t.Fatalf("violations = %d, want 1", got)
	}
}

func TestEleventhConcurrentRequestRejected(t *testing.T) {
	cfg := roomyConfig()
	cfg.Budget = 100
	cfg.MaxConcurrent = 10
	l, _ := newTestLimiter(t, cfg)

	var releases []func()
	for i := 1; i <= 10; i++ {
		d, release := l.Begin("tok", Core, false)
		if d.Status != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200", i, d.Status)
		}
		releases = append(releases, release)
	}
	if d, _ := l.Begin("tok", Core, false); d.Status != http.StatusTooManyRequests {
		t.Fatalf("11th concurrent request: status %d, want 429", d.Status)
	}
	for _, r := range releases {
		r()
	}
}

func TestWriteCostsFivePoints(t *testing.T) {
	cfg := roomyConfig()
	cfg.Budget = 100
	cfg.Points = 7
	l, c := newTestLimiter(t, cfg)

	// 7 points: a write (5) and two reads (1 each) fit; a third read doesn't.
	for i, write := range []bool{true, false, false} {
		d, release := l.Begin("tok", Core, write)
		if d.Status != http.StatusOK {
			t.Fatalf("call %d: status %d, want 200", i, d.Status)
		}
		release()
	}
	d, _ := l.Begin("tok", Core, false)
	if d.Status != http.StatusTooManyRequests || d.RetryAfter != 1 {
		t.Fatalf("over points: status %d retry-after %d; want 429, 1", d.Status, d.RetryAfter)
	}

	// After the points window and the retry-after, a write fits again.
	c.advance(time.Second)
	if d, _ := l.Begin("tok", Core, true); d.Status != http.StatusOK {
		t.Fatalf("next points window: status %d, want 200", d.Status)
	}
}

func TestTokensAreIndependent(t *testing.T) {
	l, _ := newTestLimiter(t, roomyConfig())
	for range 6 {
		doRead(l)
	}
	if d, _ := l.Begin("other", Core, false); d.Status != http.StatusOK || d.Remaining != 4 {
		t.Fatalf("other token: status %d remaining %d; want 200, 4", d.Status, d.Remaining)
	}
}

func TestSearchHasItsOwnBudget(t *testing.T) {
	cfg := roomyConfig()
	cfg.SearchBudget, cfg.SearchWindow = 2, time.Second
	l, c := newTestLimiter(t, cfg)
	if l.Resource("/search/issues") != Search || l.Resource("/repos/o/r") != Core {
		t.Fatal("paths under /search/ should be search, the rest core")
	}

	for i := range 2 {
		if d, _ := l.Begin("tok", Search, false); d.Status != http.StatusOK || d.Resource != Search || d.Limit != 2 {
			t.Fatalf("search %d: %+v, want 200 on search with limit 2", i, d)
		}
	}
	if d, _ := l.Begin("tok", Search, false); d.Status != http.StatusForbidden || !d.Violation {
		t.Fatalf("3rd search: %+v, want a 403 violation", d)
	}
	// Core is counted apart and still has its whole budget.
	if d := doRead(l); d.Status != http.StatusOK || d.Remaining != 4 {
		t.Fatalf("core while search is out: %+v, want 200 with 4 left", d)
	}
	c.advance(time.Second)
	if d, _ := l.Begin("tok", Search, false); d.Status != http.StatusOK {
		t.Fatalf("search after its reset: %+v, want 200", d)
	}

	st := l.Stats()["tok"]
	if st.Requests != 5 || st.Violations != 1 || st.Search.Requests != 4 || st.Search.OK != 3 ||
		st.Search.Primary403 != 1 || len(st.Search.WindowOK) != 2 || st.Windows != 1 {
		t.Fatalf("stats = %+v, search = %+v", st, *st.Search)
	}
}

func TestSecondaryLimitsAreSharedWithSearch(t *testing.T) {
	cfg := roomyConfig()
	cfg.MaxConcurrent = 1
	cfg.SearchBudget, cfg.SearchWindow = 30, time.Second
	l, _ := newTestLimiter(t, cfg)
	_, release := l.Begin("tok", Core, false)
	defer release()
	if d, _ := l.Begin("tok", Search, false); d.Status != http.StatusTooManyRequests {
		t.Fatalf("search while core holds the only slot: %+v, want 429", d)
	}
}

func TestValidate(t *testing.T) {
	cfg := roomyConfig()
	cfg.Window = 1500 * time.Millisecond
	if cfg.Validate() == nil {
		t.Fatal("a window that isn't whole seconds should be rejected")
	}
}
