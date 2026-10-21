// Command realtest runs the simulated agents against the real GitHub API,
// directly or through quotad, and counts what came back. It is loadgen for
// a real upstream: there is no fake server to ask for violation counts, so
// it counts every response itself.
//
// Keep the load small. A run at -demand 2 against the search API sends
// about one request a second.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/raghavs6/quotad/internal/agents"
	"github.com/raghavs6/quotad/internal/metrics"
)

// Result is one run, as printed with -json.
type Result struct {
	Strategy string          `json:"strategy"`
	Target   string          `json:"target"`
	Demand   float64         `json:"demand"`
	Counts   Counts          `json:"counts"`
	Agents   metrics.Summary `json:"agents"`
}

// Counts is every response the fleet got, by kind.
type Counts struct {
	Sent       int `json:"sent"`        // requests that got an answer
	OK         int `json:"ok"`          // 2xx
	RuleBreaks int `json:"rule_breaks"` // GitHub's own 403 or 429 for a rate limit
	Quotad429  int `json:"quotad_429"`  // quotad turned the request away itself (never reached GitHub)
	Other      int `json:"other"`       // anything else, such as a 404 or a network error
	Errors     int `json:"errors"`

	Limit    int         `json:"limit"`    // x-ratelimit-limit, as GitHub reported it
	Resource string      `json:"resource"` // x-ratelimit-resource
	WindowOK map[int]int `json:"window_ok"`

	FirstBreak string `json:"first_break,omitempty"` // what the first rule break looked like
}

// counter is a RoundTripper that sorts every response into Counts.
type counter struct {
	next http.RoundTripper
	mu   sync.Mutex
	c    Counts
}

func (t *counter) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		t.c.Errors++
		return resp, err
	}
	t.c.Sent++
	h := resp.Header
	if n, err := strconv.Atoi(h.Get("x-ratelimit-limit")); err == nil {
		t.c.Limit = n
		t.c.Resource = h.Get("x-ratelimit-resource")
	}
	switch s := resp.StatusCode; {
	case s < 300:
		t.c.OK++
		if r, err := strconv.Atoi(h.Get("x-ratelimit-reset")); err == nil {
			t.c.WindowOK[r]++
		}
	case h.Get("X-Quotad") != "":
		t.c.Quotad429++
	case (s == http.StatusForbidden || s == http.StatusTooManyRequests) &&
		(h.Get("x-ratelimit-remaining") == "0" || h.Get("retry-after") != "" || s == http.StatusTooManyRequests):
		// GitHub answers a rate limit with a 403 or 429 and either
		// remaining 0 (primary) or retry-after (secondary).
		t.c.RuleBreaks++
		if t.c.FirstBreak == "" {
			t.c.FirstBreak = fmt.Sprintf("%d remaining=%q retry-after=%q resource=%q",
				s, h.Get("x-ratelimit-remaining"), h.Get("retry-after"), h.Get("x-ratelimit-resource"))
		}
	default:
		t.c.Other++
	}
	return resp, nil
}

func main() {
	target := flag.String("target", "https://api.github.com", "base URL: GitHub, or quotad in front of it")
	path := flag.String("path", "/search/issues?q=repo:raghavs6/quotad&per_page=1", "the read-only path every call uses")
	tokenEnv := flag.String("token-env", "GITHUB_TOKEN", "environment variable holding the token (kept out of flags so it isn't in ps)")
	strategyName := flag.String("strategy", "backoff", "retry strategy: backoff or header-aware")
	nAgents := flag.Int("agents", 6, "agents sharing the token")
	demand := flag.Float64("demand", 2, "requests the fleet wants per window, as a multiple of -budget")
	budget := flag.Int("budget", 30, "GitHub's limit for -path per window (search: 30 a minute)")
	window := flag.Duration("window", time.Minute, "GitHub's window for -path")
	duration := flag.Duration("duration", 2*time.Minute, "run length")
	seed := flag.Uint64("seed", 1, "random seed")
	asJSON := flag.Bool("json", false, "print one JSON line instead of a table")
	flag.Parse()

	token := os.Getenv(*tokenEnv)
	if token == "" {
		log.Fatalf("set %s to a GitHub token", *tokenEnv)
	}
	strategy, err := agents.ParseStrategy(*strategyName)
	if err != nil {
		log.Fatal(err)
	}

	rate := *demand * float64(*budget) / window.Seconds() // requests per second, for the whole fleet
	cfg := agents.Config{
		BaseURL: *target, Token: token, Strategy: strategy,
		Agents: *nAgents, TaskGap: time.Duration(float64(*nAgents) / rate * float64(time.Second)),
		ReadPath:    *path,
		BaseBackoff: time.Second, MaxBackoff: 32 * time.Second, Seed: *seed,
	}

	cnt := &counter{next: http.DefaultTransport, c: Counts{WindowOK: map[int]int{}}}
	client := &http.Client{Transport: cnt}
	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	start := time.Now()
	rec := metrics.NewRecorder(start)
	agents.Run(ctx, cfg, client, rec)

	res := Result{Strategy: strategy.String(), Target: *target, Demand: *demand, Counts: cnt.c, Agents: rec.Summarize(start.Add(*duration))}
	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(res)
		return
	}
	c, a := res.Counts, res.Agents
	fmt.Printf("strategy %s via %s, demand %.2gx of %d per %v, %d agents, %v\n",
		res.Strategy, *target, *demand, *budget, *window, *nAgents, *duration)
	fmt.Printf("  GitHub rule breaks  %d (first: %s)\n", c.RuleBreaks, c.FirstBreak)
	fmt.Printf("  responses           %d sent, %d ok, %d quotad 429, %d other, %d errors\n", c.Sent, c.OK, c.Quotad429, c.Other, c.Errors)
	fmt.Printf("  limit seen          %d (%s)\n", c.Limit, c.Resource)
	resets := make([]int, 0, len(c.WindowOK))
	for r := range c.WindowOK {
		resets = append(resets, r)
	}
	slices.Sort(resets)
	for _, r := range resets {
		fmt.Printf("  window to %s   %d ok\n", time.Unix(int64(r), 0).Format("15:04:05"), c.WindowOK[r])
	}
	fmt.Printf("  critical wait       p50 %.2fs, p95 %.2fs (%d done, %d unfinished)\n",
		a.CriticalP50, a.CriticalP95, a.CriticalDone, a.CriticalUnfinished)
}
