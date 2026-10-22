// Command quotad is the rate-limit governor proxy. Agents point their API
// base URL at it. It learns each token's primary budgets, one per resource
// (core, search, ...), from the upstream's x-ratelimit-* headers, keeps each
// token under the secondary limits, and holds requests until they fit.
//
// With -redis, every quotad on the same Redis shares one budget per token,
// so copies on many machines stay under the limits together.
//
// Every flag can also be set by an environment variable: QUOTAD_ plus the
// flag name in capitals with - as _ (for example QUOTAD_MAX_HOLD=1m). A flag
// on the command line wins over the variable.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/raghavs6/quotad/internal/budget"
	"github.com/raghavs6/quotad/internal/proxy"
	"github.com/redis/go-redis/v9"
)

// version is set at release build time with -ldflags "-X main.version=...".
var version = "dev"

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
	showVersion := flag.Bool("version", false, "print the version and exit")
	if err := flagsFromEnv(flag.CommandLine, os.LookupEnv); err != nil {
		log.Fatal(err)
	}
	flag.Parse()
	// go install pkg@v0.1.0 builds record the module version instead.
	if bi, ok := debug.ReadBuildInfo(); ok && version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
	if *showVersion {
		fmt.Println("quotad", version)
		return
	}

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
	p.Version = version
	log.Printf("quotad %s on %s -> %s (max hold %v, reset guard %v, %d in flight, %d points/%v, min pause %v)",
		version, *listen, upstream, *maxHold, *resetGuard, *maxInFlight, *points, *pointsWindow, *minPause)
	log.Fatal(http.ListenAndServe(*listen, p))
}

// envName is the environment variable for a flag: QUOTAD_MAX_HOLD for max-hold.
func envName(flagName string) string {
	return "QUOTAD_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// flagsFromEnv sets each flag in fs from its environment variable, if set.
// Call it before fs.Parse, so the command line still wins.
func flagsFromEnv(fs *flag.FlagSet, lookup func(string) (string, bool)) error {
	var err error
	fs.VisitAll(func(f *flag.Flag) {
		v, ok := lookup(envName(f.Name))
		if !ok || err != nil {
			return
		}
		if e := f.Value.Set(v); e != nil {
			err = fmt.Errorf("%s=%q: %v", envName(f.Name), v, e)
		}
	})
	return err
}
