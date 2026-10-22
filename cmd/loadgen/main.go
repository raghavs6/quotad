// Command loadgen runs a fleet of simulated agents sharing one token against
// a GitHub-like API (the fake server, or later the proxy in front of it),
// then prints what happened.
//
// Load is set as -demand: how many requests per window the fleet would send
// if nothing were ever rejected, as a multiple of the budget. Below 1 there
// is nothing to fix; the interesting runs are at 1 and above. Searches,
// which GitHub counts apart from core, are set the same way with
// -search-demand, as a multiple of the search budget.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/raghavs6/quotad/internal/agents"
	"github.com/raghavs6/quotad/internal/fake"
	"github.com/raghavs6/quotad/internal/metrics"
)

// Result is one run, as printed with -json.
type Result struct {
	Strategy     string          `json:"strategy"`
	Demand       float64         `json:"demand"`
	SearchDemand float64         `json:"search_demand,omitempty"`
	Seed         uint64          `json:"seed"`
	Server       fake.Stats      `json:"server"`
	Utilization  float64         `json:"utilization"` // successful core requests ÷ budget, over windows that fit in the run
	SearchUtil   float64         `json:"search_utilization,omitempty"`
	Agents       metrics.Summary `json:"agents"`
}

func main() {
	target := flag.String("target", "http://127.0.0.1:8081", "base URL of the API (fake server or proxy); several, comma-separated, spread the agents across them")
	statsURL := flag.String("stats", "", "URL of the fake server's /__stats (default: target + /__stats)")
	token := flag.String("token", "shared", "the one token every agent uses")
	strategyName := flag.String("strategy", "backoff", "retry strategy: backoff or header-aware")
	nAgents := flag.Int("agents", 10, "agents, each running a task stream of critical calls")
	pollers := flag.Int("pollers", 5, "how many of the agents also poll CI (background calls)")
	demand := flag.Float64("demand", 2, "requests the fleet wants per window, as a multiple of the budget")
	bgShare := flag.Float64("background-share", 0.3, "share of demand that is background polling")
	writeFrac := flag.Float64("write-frac", 0.1, "share of task calls that are writes")
	budget := flag.Int("budget", 500, "the server's budget per window (must match fakegithub)")
	window := flag.Duration("window", 60*time.Second, "the server's window (must match fakegithub)")
	latency := flag.Duration("latency", 100*time.Millisecond, "the server's service time (must match fakegithub)")
	searchDemand := flag.Float64("search-demand", 0, "searches the fleet wants, as a multiple of the search budget (0 = none)")
	searchBudget := flag.Int("search-budget", 0, "the server's search budget per search window (must match fakegithub; 0 = 30*budget/5000)")
	searchWindow := flag.Duration("search-window", 0, "the server's search window (must match fakegithub; 0 = window/60)")
	windows := flag.Int("windows", 3, "run length, in windows")
	seed := flag.Uint64("seed", 1, "random seed")
	asJSON := flag.Bool("json", false, "print one JSON line instead of a table")
	flag.Parse()

	strategy, err := agents.ParseStrategy(*strategyName)
	if err != nil {
		log.Fatal(err)
	}
	if *pollers > *nAgents {
		log.Fatalf("pollers (%d) can't exceed agents (%d)", *pollers, *nAgents)
	}

	if *searchBudget == 0 {
		*searchBudget = max(1, 30**budget/5000)
	}
	if *searchWindow == 0 {
		*searchWindow = *window / 60
	}

	// Turn demand into timing. Task calls run on a schedule, one every
	// taskGap per agent. Searches are task calls too. Polls are
	// sequential, so one poll cycle is latency + pollEvery.
	rate := *demand * float64(*budget) / window.Seconds() // core requests per second
	searchRate := *searchDemand * float64(*searchBudget) / searchWindow.Seconds()
	taskRate := rate*(1-*bgShare) + searchRate
	taskGap := time.Duration(float64(*nAgents) / taskRate * float64(time.Second))
	pollEvery := time.Duration(0)
	if *pollers > 0 {
		pollEvery = time.Duration(float64(*pollers)/(rate**bgShare)*float64(time.Second)) - *latency
	}
	if taskGap < *latency || pollEvery < 0 {
		log.Fatalf("demand %.2g needs more agents or pollers: at %v latency each can't send that fast", *demand, *latency)
	}

	cfg := agents.Config{
		BaseURL: *target, Token: *token, Strategy: strategy,
		Agents: *nAgents, Pollers: *pollers, TaskGap: taskGap, PollEvery: pollEvery, WriteFrac: *writeFrac,
		SearchFrac:  searchRate / taskRate,
		BaseBackoff: time.Second, MaxBackoff: 32 * time.Second, Seed: *seed,
	}

	// Start on a whole second so the run lines up with the server's windows
	// (they start on whole seconds too), and run for exactly -windows of them.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*windows)**window)
	defer cancel()
	rec := metrics.NewRecorder(start)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 2 * *nAgents}}
	agents.Run(ctx, cfg, client, rec)
	end := start.Add(time.Duration(*windows) * *window)

	if *statsURL == "" {
		*statsURL = *target + "/__stats"
	}
	server, err := fetchStats(*statsURL, "token "+*token)
	if err != nil {
		log.Fatal(err)
	}
	res := Result{Strategy: strategy.String(), Demand: *demand, SearchDemand: *searchDemand, Seed: *seed, Server: server, Agents: rec.Summarize(end)}
	res.Utilization = utilization(server.WindowResets, server.WindowOK, *budget, end)
	if s := server.Search; s != nil {
		res.SearchUtil = utilization(s.WindowResets, s.WindowOK, *searchBudget, end)
	}

	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(res)
		return
	}
	a := res.Agents
	fmt.Printf("strategy %s, demand %.2gx, %d agents (%d polling), %d windows of %v\n",
		res.Strategy, *demand, *nAgents, *pollers, *windows, *window)
	fmt.Printf("  violations          %d\n", server.Violations)
	fmt.Printf("  rejections          %d (403: %d, 429: %d), peak %d/s, %.0f%% within %v of a reset\n",
		server.Primary403+server.Secondary429, server.Primary403, server.Secondary429,
		a.PeakRejectionsPerS, 100*a.PostResetShare, metrics.PostResetSpan)
	fmt.Printf("  critical wait       p50 %.2fs, p95 %.2fs (%d done, %d unfinished)\n",
		a.CriticalP50, a.CriticalP95, a.CriticalDone, a.CriticalUnfinished)
	for _, name := range []string{"core", "search"} {
		if r, ok := a.Resources[name]; ok {
			fmt.Printf("    %-6s            wait p50 %.2fs, p95 %.2fs; call p50 %.2fs, p95 %.2fs (%d done, %d unfinished)\n",
				name, r.WaitP50, r.WaitP95, r.CallP50, r.CallP95, r.Done, r.Unfinished)
		}
	}
	fmt.Printf("  utilization         %.0f%% (%d ok, %d windows started)\n", 100*res.Utilization, server.OK, server.Windows)
	if s := server.Search; s != nil {
		fmt.Printf("  search              %d ok, %d 403s, %.0f%% utilization\n", s.OK, s.Primary403, 100*res.SearchUtil)
	}
	fmt.Printf("  rejections per sec  %v\n", a.RejectionsPerS)
}

// utilization is successful requests ÷ budget, over the windows that both
// started and reset within the run. Windows start on the first request
// after a reset, so they drift with traffic and the last one is often cut
// off by the end of the run; counting it would charge a full window's
// budget for a few seconds of traffic.
func utilization(resets []int64, oks []int, budget int, end time.Time) float64 {
	ok, full := 0, 0
	for i, reset := range resets {
		if reset <= end.Unix() {
			ok += oks[i]
			full++
		}
	}
	if full == 0 {
		return 0
	}
	return float64(ok) / float64(budget*full)
}

func fetchStats(url, token string) (fake.Stats, error) {
	resp, err := http.Get(url)
	if err != nil {
		return fake.Stats{}, err
	}
	defer resp.Body.Close()
	var all map[string]fake.Stats
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		return fake.Stats{}, fmt.Errorf("decoding %s: %w", url, err)
	}
	return all[token], nil
}
