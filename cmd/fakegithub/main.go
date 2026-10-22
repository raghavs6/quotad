// Command fakegithub runs a fake GitHub REST API that enforces GitHub's rate
// limits with sizes and windows set by flags.
//
// The defaults shrink GitHub's hour to 60s and scale every other limit by
// the same factor: 5000 requests/hour becomes -budget per -window, and the
// 900 points/minute secondary limit becomes 900*budget/5000 points per
// window/60, so the primary and secondary limits keep their real ratio.
// Search's 30 per minute is scaled the same way: 30*budget/5000 per
// window/60.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/raghavs6/quotad/internal/fake"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8081", "listen address")
	budget := flag.Int("budget", 500, "primary limit: requests per window, per token")
	window := flag.Duration("window", 60*time.Second, "primary window (GitHub: 1h); whole seconds")
	concurrency := flag.Int("concurrency", 10, "secondary limit: max requests in flight per token (GitHub: 100)")
	points := flag.Int("points", 0, "secondary limit: points per points window (0 = 900*budget/5000)")
	pointsWindow := flag.Duration("points-window", 0, "points window (GitHub: 1m; 0 = window/60)")
	searchBudget := flag.Int("search-budget", 0, "search's own primary limit per search window (0 = 30*budget/5000)")
	searchWindow := flag.Duration("search-window", 0, "search window (GitHub: 1m; 0 = window/60); whole seconds")
	latency := flag.Duration("latency", 100*time.Millisecond, "service time of each admitted request")
	flag.Parse()

	cfg := fake.Config{
		Budget:        *budget,
		Window:        *window,
		MaxConcurrent: *concurrency,
		Points:        *points,
		PointsWindow:  *pointsWindow,
		SearchBudget:  *searchBudget,
		SearchWindow:  *searchWindow,
	}
	if cfg.Points == 0 {
		cfg.Points = max(fake.WriteCost, 900**budget/5000)
	}
	if cfg.PointsWindow == 0 {
		cfg.PointsWindow = *window / 60
	}
	if cfg.SearchBudget == 0 {
		cfg.SearchBudget = max(1, 30**budget/5000)
	}
	if cfg.SearchWindow == 0 {
		cfg.SearchWindow = *window / 60
	}

	l, err := fake.NewLimiter(cfg, time.Now)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fakegithub on %s: %d req/%v, %d searches/%v, %d in flight, %d points/%v, latency %v",
		*addr, cfg.Budget, cfg.Window, cfg.SearchBudget, cfg.SearchWindow, cfg.MaxConcurrent, cfg.Points, cfg.PointsWindow, *latency)
	log.Fatal(http.ListenAndServe(*addr, fake.NewServer(l, *latency)))
}
