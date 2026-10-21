package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/raghavs6/quotad/internal/budget"
	"github.com/raghavs6/quotad/internal/fake"
)

// These tests put the proxy in front of the real fake server with a 1s
// window, so they take about a second of real time.

func setup(t *testing.T, budgetPerWindow int, cfg budget.Config) (*httptest.Server, *fake.Limiter) {
	t.Helper()
	return setupFake(t, fake.Config{Budget: budgetPerWindow, Window: time.Second, MaxConcurrent: 100, Points: 1000, PointsWindow: time.Second}, cfg)
}

func setupFake(t *testing.T, fc fake.Config, cfg budget.Config) (*httptest.Server, *fake.Limiter) {
	t.Helper()
	l, err := fake.NewLimiter(fc, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(fake.NewServer(l, 10*time.Millisecond))
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	p := httptest.NewServer(New(u, cfg))
	t.Cleanup(p.Close)
	return p, l
}

func get(t *testing.T, base string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/repos/o/r", nil)
	req.Header.Set("Authorization", "token t")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestHoldsInsteadOfOvershooting(t *testing.T) {
	p, l := setup(t, 3, budget.Config{MaxHold: 3 * time.Second})

	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = get(t, p.URL).StatusCode
		}()
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d: status %d, want 200 (held until a reset, not rejected)", i, c)
		}
	}
	st := l.Stats()["token t"]
	if st.Violations != 0 || st.Primary403 != 0 || st.OK != 8 {
		t.Fatalf("server stats = %+v; want 8 ok and no violations", st)
	}
}

func TestTurnsAwayLongWaitsWithoutTouchingUpstream(t *testing.T) {
	// A window of 1s but a max hold of 0: once the 2 units are spent, the
	// proxy must answer 429 itself.
	p, l := setup(t, 2, budget.Config{MaxHold: 0})
	get(t, p.URL)
	get(t, p.URL)
	resp := get(t, p.URL)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "1" || resp.Header.Get("X-Quotad") == "" {
		t.Fatalf("third request: %d %v; want quotad's 429 with Retry-After 1", resp.StatusCode, resp.Header)
	}
	if st := l.Stats()["token t"]; st.Requests != 2 || st.Violations != 0 {
		t.Fatalf("server stats = %+v; want only the 2 requests that fit", st)
	}
}

func TestPassesResponseThrough(t *testing.T) {
	p, _ := setup(t, 5, budget.Config{MaxHold: time.Second})
	resp := get(t, p.URL)
	if resp.StatusCode != 200 || resp.Header.Get("x-ratelimit-remaining") != "4" {
		t.Fatalf("got %d remaining %q; want upstream's 200 and headers unchanged", resp.StatusCode, resp.Header.Get("x-ratelimit-remaining"))
	}
}

func TestInFlightCapAvoidsSecondary429s(t *testing.T) {
	// The fake server allows 2 in flight. 8 requests arrive at once.
	p, l := setupFake(t,
		fake.Config{Budget: 100, Window: 60 * time.Second, MaxConcurrent: 2, Points: 1000, PointsWindow: time.Second},
		budget.Config{MaxHold: 5 * time.Second, MaxInFlight: 2})

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c := get(t, p.URL).StatusCode; c != http.StatusOK {
				t.Errorf("status %d, want 200", c)
			}
		}()
	}
	wg.Wait()
	if st := l.Stats()["token t"]; st.Secondary429 != 0 || st.Violations != 0 || st.OK != 8 {
		t.Fatalf("server stats = %+v; want 8 ok, no 429s", st)
	}
}

func TestSecondary429PausesTheToken(t *testing.T) {
	// An upstream that answers the first request with a 429 and retry-after 1.
	var mu sync.Mutex
	var times []time.Time
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		first := len(times) == 1
		mu.Unlock()
		w.Header().Set("x-ratelimit-limit", "100")
		w.Header().Set("x-ratelimit-remaining", "90")
		w.Header().Set("x-ratelimit-reset", "9999999999")
		if first {
			w.Header().Set("retry-after", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	p := httptest.NewServer(New(u, budget.Config{MaxHold: 5 * time.Second}))
	defer p.Close()

	if c := get(t, p.URL).StatusCode; c != http.StatusTooManyRequests {
		t.Fatalf("first: %d, want the upstream's 429 passed through", c)
	}
	if c := get(t, p.URL).StatusCode; c != http.StatusOK {
		t.Fatalf("second: %d, want 200 after the pause", c)
	}
	mu.Lock()
	defer mu.Unlock()
	if gap := times[1].Sub(times[0]); gap < time.Second {
		t.Fatalf("second request reached the upstream %v after the 429, want at least 1s", gap)
	}
}
