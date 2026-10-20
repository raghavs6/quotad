// Command quotad is the rate-limit governor proxy. Agents point their API
// base URL at it; it holds requests when their token's budget is out and
// learns the budget from the upstream's x-ratelimit-* headers.
package main

import (
	"flag"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/raghavs6/quotad/internal/budget"
	"github.com/raghavs6/quotad/internal/proxy"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "listen address")
	upstreamURL := flag.String("upstream", "https://api.github.com", "upstream API base URL")
	maxHold := flag.Duration("max-hold", 30*time.Second, "longest a request waits for budget before an immediate 429")
	resetGuard := flag.Duration("reset-guard", time.Second, "extra wait past the upstream's reset time, for clock skew")
	flag.Parse()

	upstream, err := url.Parse(*upstreamURL)
	if err != nil {
		log.Fatal(err)
	}
	p := proxy.New(upstream, budget.Config{MaxHold: *maxHold, ResetGuard: *resetGuard})
	log.Printf("quotad on %s -> %s (max hold %v, reset guard %v)", *listen, upstream, *maxHold, *resetGuard)
	log.Fatal(http.ListenAndServe(*listen, p))
}
