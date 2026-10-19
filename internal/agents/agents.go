// Package agents simulates coding agents that share one GitHub token, each
// retrying on its own with one of the two baseline strategies.
package agents

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/raghavs6/quotad/internal/metrics"
)

// Strategy is how an agent reacts to rate limits, on its own.
type Strategy int

const (
	// Backoff waits base, 2*base, 4*base... plus random jitter after each
	// 403 or 429, ignoring headers. This is what most SDKs do.
	Backoff Strategy = iota
	// HeaderAware also obeys retry-after and, when x-ratelimit-remaining
	// hits 0, pauses until x-ratelimit-reset. It's the best one agent can
	// do alone, without knowing what the other agents are doing.
	HeaderAware
)

func (s Strategy) String() string {
	switch s {
	case Backoff:
		return "backoff"
	case HeaderAware:
		return "header-aware"
	}
	return fmt.Sprintf("Strategy(%d)", int(s))
}

// ParseStrategy turns a flag value into a Strategy.
func ParseStrategy(s string) (Strategy, error) {
	switch s {
	case "backoff":
		return Backoff, nil
	case "header-aware":
		return HeaderAware, nil
	}
	return 0, fmt.Errorf("unknown strategy %q (want backoff or header-aware)", s)
}

// Config describes the fleet.
type Config struct {
	BaseURL  string
	Token    string
	Strategy Strategy

	Agents    int           // agents running a task stream (critical calls)
	Pollers   int           // the first Pollers agents also poll CI (background calls)
	TaskPause time.Duration // mean pause between an agent's task calls
	PollEvery time.Duration // pause between a poller's calls
	WriteFrac float64       // share of task calls that are writes

	BaseBackoff time.Duration // first backoff step
	MaxBackoff  time.Duration // backoff cap
	Seed        uint64
}

// Run starts the fleet and blocks until ctx is done and every agent has
// stopped. Critical calls still in progress at the end are recorded as
// unfinished.
func Run(ctx context.Context, cfg Config, client *http.Client, rec *metrics.Recorder) {
	var wg sync.WaitGroup
	for i := range cfg.Agents {
		a := &agent{cfg: cfg, client: client, rec: rec, remaining: -1}
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.taskStream(ctx, rand.New(rand.NewPCG(cfg.Seed, uint64(2*i))))
		}()
		if i < cfg.Pollers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				a.poll(ctx, rand.New(rand.NewPCG(cfg.Seed, uint64(2*i+1))))
			}()
		}
	}
	wg.Wait()
}

type agent struct {
	cfg    Config
	client *http.Client
	rec    *metrics.Recorder

	// What this agent alone has learned from headers (HeaderAware only).
	// Its task stream and poller share it, as one agent's code would.
	mu         sync.Mutex
	remaining  int // -1 until the first response
	reset      time.Time
	retryUntil time.Time
}

// taskStream sends critical calls one after another: mostly reads, some
// writes, with a short pause between them.
func (a *agent) taskStream(ctx context.Context, rng *rand.Rand) {
	for ctx.Err() == nil {
		method, path := http.MethodGet, "/repos/o/r/contents/file.go"
		if rng.Float64() < a.cfg.WriteFrac {
			method, path = http.MethodPost, "/repos/o/r/issues/1/comments"
		}
		wanted := time.Now()
		if a.do(ctx, rng, method, path) {
			a.rec.Critical(wanted, time.Now())
		} else {
			a.rec.Critical(wanted, time.Time{})
			return
		}
		// Uniform in [0.5, 1.5) × TaskPause keeps the mean at TaskPause.
		sleep(ctx, time.Duration((0.5+rng.Float64())*float64(a.cfg.TaskPause)))
	}
}

// poll asks "is CI done?" every PollEvery. These are background calls.
func (a *agent) poll(ctx context.Context, rng *rand.Rand) {
	for ctx.Err() == nil {
		if !a.do(ctx, rng, http.MethodGet, "/repos/o/r/actions/runs") {
			return
		}
		sleep(ctx, a.cfg.PollEvery)
	}
}

// do sends one call, retrying until it succeeds. It returns false only if
// ctx ended first.
func (a *agent) do(ctx context.Context, rng *rand.Rand, method, path string) bool {
	for attempt := 0; ; attempt++ {
		if a.cfg.Strategy == HeaderAware {
			if !sleep(ctx, time.Until(a.blockedUntil())) {
				return false
			}
		}

		status, h, err := a.send(ctx, method, path)
		if ctx.Err() != nil {
			return false
		}
		if err == nil {
			if reset, err := strconv.ParseInt(h.Get("x-ratelimit-reset"), 10, 64); err == nil {
				a.rec.Reset(reset)
			}
			if status < 300 {
				a.observe(h, 0)
				return true
			}
			a.rec.Rejection(time.Now())
		}

		if a.cfg.Strategy == HeaderAware && err == nil {
			retryAfter, _ := strconv.Atoi(h.Get("retry-after"))
			a.observe(h, retryAfter)
			if retryAfter > 0 || h.Get("x-ratelimit-remaining") == "0" {
				continue // blockedUntil now says how long to wait
			}
		}
		if !sleep(ctx, a.backoff(rng, attempt)) {
			return false
		}
	}
}

func (a *agent) send(ctx context.Context, method, path string) (int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.cfg.BaseURL+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "token "+a.cfg.Token)
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, resp.Header, nil
}

// backoff is base × 2^attempt, capped, plus up to the same again in jitter.
func (a *agent) backoff(rng *rand.Rand, attempt int) time.Duration {
	d := a.cfg.BaseBackoff << min(attempt, 30)
	if d <= 0 || d > a.cfg.MaxBackoff {
		d = a.cfg.MaxBackoff
	}
	return d + time.Duration(rng.Int64N(int64(d)))
}

// observe stores what one response said. Responses can arrive out of
// order; an agent on its own just trusts the latest one.
func (a *agent) observe(h http.Header, retryAfter int) {
	if a.cfg.Strategy != HeaderAware {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if n, err := strconv.Atoi(h.Get("x-ratelimit-remaining")); err == nil {
		a.remaining = n
	}
	if r, err := strconv.ParseInt(h.Get("x-ratelimit-reset"), 10, 64); err == nil {
		a.reset = time.Unix(r, 0)
	}
	if retryAfter > 0 {
		a.retryUntil = time.Now().Add(time.Duration(retryAfter) * time.Second)
	}
}

// blockedUntil is when this agent believes it may send again.
func (a *agent) blockedUntil() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	until := a.retryUntil
	if a.remaining == 0 && a.reset.After(until) {
		until = a.reset
	}
	return until
}

// sleep waits for d or until ctx is done. It reports whether ctx is still live.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
