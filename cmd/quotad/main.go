// Command quotad is the rate-limit governor proxy. Agents point their API
// base URL at it. It learns each token's primary budgets, one per resource
// (core, search, ...), from the upstream's x-ratelimit-* headers, keeps each
// token under the secondary limits, and holds requests until they fit.
//
// With -redis, every quotad on the same Redis shares one budget per token,
// so copies on many machines stay under the limits together.
package main

import (
	"flag"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/raghavs6/quotad/internal/budget"
	"github.com/raghavs6/quotad/internal/proxy"
	"github.com/redis/go-redis/v9"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "listen address")
	upstreamURL := flag.String("upstream", "https://api.github.com", "upstream API base URL")
	maxHold := flag.Duration("max-hold", 30*time.Second, "longest a request waits for budget before an immediate 429")
	resetGuard := flag.Duration("reset-guard", time.Second, "extra wait past the upstream's reset time, for clock skew")
	// GitHub doesn't send its secondary limits in headers, so they're flags
	// that default to GitHub's published numbers.
	maxInFlight := flag.Int("max-in-flight", budget.GitHubMaxInFlight, "secondary limit: requests sent but not yet answered, per token")
	points := flag.Int("points", budget.GitHubPoints, "secondary limit: points per -points-window, per token (read 1, write 5)")
	pointsWindow := flag.Duration("points-window", budget.GitHubPointsEvery, "secondary limit: the points window")
	minPause := flag.Duration("min-pause", budget.GitHubMinPause, "pause after a secondary-limit answer without retry-after; doubles while they repeat")
	redisURL := flag.String("redis", "", "Redis URL (redis://host:6379/0) to share budgets with other quotad copies; empty keeps them in memory")
	lease := flag.Duration("lease", budget.DefaultLease, "with -redis: how long a request may go unanswered before it counts as failed (frees a crashed copy's slots)")
	redisTimeout := flag.Duration("redis-timeout", 100*time.Millisecond, "with -redis: longest wait for Redis before counting alone for a moment")
	flag.Parse()

	upstream, err := url.Parse(*upstreamURL)
	if err != nil {
		log.Fatal(err)
	}
	cfg := budget.Config{
		MaxHold: *maxHold, ResetGuard: *resetGuard,
		MaxInFlight: *maxInFlight, Points: *points, PointsWindow: *pointsWindow, MinPause: *minPause,
		Lease: *lease,
	}
	var rdb *redis.Client
	if *redisURL != "" {
		opt, err := redis.ParseURL(*redisURL)
		if err != nil {
			log.Fatal(err)
		}
		opt.DialTimeout, opt.ReadTimeout, opt.WriteTimeout = *redisTimeout, *redisTimeout, *redisTimeout
		opt.MaxRetries = -1 // a slow Redis is a missing Redis: count alone, don't retry
		rdb = redis.NewClient(opt)
		log.Printf("quotad: sharing budgets through Redis at %s", opt.Addr)
	}
	p := proxy.New(upstream, cfg, rdb)
	log.Printf("quotad on %s -> %s (max hold %v, reset guard %v, %d in flight, %d points/%v, min pause %v)",
		*listen, upstream, *maxHold, *resetGuard, *maxInFlight, *points, *pointsWindow, *minPause)
	log.Fatal(http.ListenAndServe(*listen, p))
}
