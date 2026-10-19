package agents

import (
	"context"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/raghavs6/quotad/internal/fake"
	"github.com/raghavs6/quotad/internal/metrics"
)

// These tests run agents against the real fake server with a 1s window, so
// each takes about a second of real time.

func setup(t *testing.T, cfg fake.Config, strategy Strategy) (*agent, *fake.Limiter) {
	t.Helper()
	l, err := fake.NewLimiter(cfg, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(fake.NewServer(l, 0))
	t.Cleanup(srv.Close)
	a := &agent{
		cfg: Config{
			BaseURL: srv.URL, Token: "t", Strategy: strategy,
			BaseBackoff: 50 * time.Millisecond, MaxBackoff: 200 * time.Millisecond,
		},
		client:    srv.Client(),
		rec:       metrics.NewRecorder(time.Now()),
		remaining: -1,
	}
	return a, l
}

// waitForFreshWindow starts the test just after a whole second, so the
// fake server's first window lasts close to a full second.
func waitForFreshWindow() {
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second + 10*time.Millisecond)))
}

func TestHeaderAwarePausesUntilReset(t *testing.T) {
	a, l := setup(t, fake.Config{Budget: 2, Window: time.Second, MaxConcurrent: 10, Points: 100, PointsWindow: time.Second}, HeaderAware)
	rng := rand.New(rand.NewPCG(1, 1))
	waitForFreshWindow()

	for i := range 3 {
		if !a.do(context.Background(), rng, http.MethodGet, "/x") {
			t.Fatalf("call %d failed", i)
		}
	}
	st := l.Stats()["token t"]
	// It saw remaining 0 after call 2 and waited for the reset instead of
	// sending call 3 into a 403.
	if st.Violations != 0 || st.Primary403 != 0 || st.OK != 3 || st.Windows != 2 {
		t.Fatalf("stats = %+v; want 0 violations, 0 403s, 3 ok across 2 windows", st)
	}
}

func TestHeaderAwareObeysRetryAfter(t *testing.T) {
	a, l := setup(t, fake.Config{Budget: 100, Window: 10 * time.Second, MaxConcurrent: 10, Points: 5, PointsWindow: 500 * time.Millisecond}, HeaderAware)
	rng := rand.New(rand.NewPCG(1, 1))

	begin := time.Now()
	for range 2 {
		if !a.do(context.Background(), rng, http.MethodPost, "/x") {
			t.Fatal("write failed")
		}
	}
	// The second write hit the points limit, got retry-after: 1, and waited
	// the full second even though backoff alone would have retried at 50ms.
	if el := time.Since(begin); el < time.Second {
		t.Fatalf("second write finished after %v, want at least 1s", el)
	}
	if st := l.Stats()["token t"]; st.Secondary429 != 1 || st.Violations != 0 {
		t.Fatalf("stats = %+v; want one 429 and no violations", st)
	}
}

func TestBackoffIgnoresHeaders(t *testing.T) {
	a, l := setup(t, fake.Config{Budget: 2, Window: time.Second, MaxConcurrent: 10, Points: 100, PointsWindow: time.Second}, Backoff)
	rng := rand.New(rand.NewPCG(1, 1))
	waitForFreshWindow()

	for i := range 3 {
		if !a.do(context.Background(), rng, http.MethodGet, "/x") {
			t.Fatalf("call %d failed", i)
		}
	}
	// Call 3 kept retrying into the 403 until the window reset.
	if st := l.Stats()["token t"]; st.Violations == 0 || st.OK != 3 {
		t.Fatalf("stats = %+v; want violations > 0 and 3 ok", st)
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	a := &agent{cfg: Config{BaseBackoff: time.Second, MaxBackoff: 4 * time.Second}}
	rng := rand.New(rand.NewPCG(1, 1))
	for attempt, base := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		d := a.backoff(rng, attempt)
		if d < base || d >= 2*base {
			t.Errorf("attempt %d: %v, want in [%v, %v)", attempt, d, base, 2*base)
		}
	}
}

func TestRunStopsAndRecordsUnfinished(t *testing.T) {
	l, _ := fake.NewLimiter(fake.Config{Budget: 1000, Window: 60 * time.Second, MaxConcurrent: 100, Points: 1000, PointsWindow: time.Second}, time.Now)
	srv := httptest.NewServer(fake.NewServer(l, 300*time.Millisecond))
	defer srv.Close()

	start := time.Now()
	rec := metrics.NewRecorder(start)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	Run(ctx, Config{
		BaseURL: srv.URL, Token: "t", Strategy: HeaderAware,
		Agents: 3, Pollers: 1, TaskPause: 10 * time.Millisecond, PollEvery: 10 * time.Millisecond,
		BaseBackoff: time.Second, MaxBackoff: time.Second, Seed: 1,
	}, srv.Client(), rec)

	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Run took %v after a 500ms deadline", el)
	}
	s := rec.Summarize(time.Now())
	// Each agent finishes one 300ms call, then is cut off mid-call.
	if s.CriticalDone != 3 || s.CriticalUnfinished != 3 {
		t.Fatalf("done %d unfinished %d, want 3 and 3", s.CriticalDone, s.CriticalUnfinished)
	}
}
