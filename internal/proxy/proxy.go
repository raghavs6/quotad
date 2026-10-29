// Package proxy is quotad's HTTP layer: it holds each request until its
// token's limits allow it, forwards it to the upstream, and feeds the
// response back into the budget.
package proxy

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/raghavs6/quotad/internal/budget"
	"github.com/redis/go-redis/v9"
)

// Proxy forwards to one upstream, with one budget per token.
type Proxy struct {
	cfg      budget.Config
	now      func() time.Time
	schedule func(time.Duration, func())
	rdb      *redis.Client // nil: budgets live in this process
	rp       *httputil.ReverseProxy
	upstream *url.URL
	n        counters

	// Version is shown on the health and stats pages.
	Version string

	mu      sync.Mutex
	budgets map[string]*budget.Budget // keyed by the Authorization header
}

type ticketKey struct{}

// held is what a request carries through the reverse proxy: its ticket and
// the resource it was guessed to count against.
type held struct {
	t   *budget.Ticket
	res string
}

// New returns a proxy to upstream. With rdb nil, each token's budget lives
// in this process. Otherwise it lives in Redis, shared with every quotad
// that uses the same Redis.
func New(upstream *url.URL, cfg budget.Config, rdb *redis.Client) *Proxy {
	if rdb != nil && cfg.Lease == 0 {
		cfg.Lease = budget.DefaultLease
	}
	p := &Proxy{cfg: cfg, now: time.Now, schedule: budget.AfterFunc, rdb: rdb, budgets: map[string]*budget.Budget{}, upstream: upstream, Version: "dev"}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
		},
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
		ModifyResponse: func(resp *http.Response) error {
			h := heldFor(resp.Request.Context())
			if got := resp.Header.Get("x-ratelimit-resource"); got != "" && got != h.res {
				log.Printf("quotad: %s %s counted against %q, guessed %q", resp.Request.Method, resp.Request.URL.Path, got, h.res)
			}
			h.t.Done(resp.StatusCode, resp.Header)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// No usable answer: the upstream may still have counted the request.
			heldFor(r.Context()).t.Failed()
			if r.Context().Err() == nil {
				w.WriteHeader(http.StatusBadGateway)
			}
		},
	}
	return p
}

func heldFor(ctx context.Context) held {
	return ctx.Value(ticketKey{}).(held)
}

// classifyResource guesses which of GitHub's rate-limit resources a request
// counts against, from its path. It is only a guess: the response's
// x-ratelimit-resource header corrects the budget either way. Rare
// resources (dependency_snapshots, audit_log, ...) fall under core.
func classifyResource(path string) string {
	switch {
	case path == "/search/code":
		return "code_search"
	case path == "/search" || strings.HasPrefix(path, "/search/"):
		return "search"
	case path == "/graphql":
		return "graphql"
	default:
		return "core"
	}
}

func (p *Proxy) budgetFor(token string) *budget.Budget {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.budgets[token]
	if b == nil {
		if p.rdb != nil {
			b = budget.NewRedis(p.cfg, p.rdb, token, p.schedule)
		} else {
			b = budget.New(p.cfg, p.now, p.schedule)
		}
		p.budgets[token] = b
	}
	return b
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, StatusPrefix) {
		p.serveStatus(w, r)
		return
	}
	p.n.requests.Add(1)
	// Like GitHub's points rule: GET, HEAD and OPTIONS are reads, the rest writes.
	write := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
	res := classifyResource(r.URL.Path)
	t, err := p.budgetFor(r.Header.Get("Authorization")).Acquire(r.Context(), res, write)
	var tooLong *budget.TooLongError
	switch {
	case errors.As(err, &tooLong):
		p.n.turnedAway.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", strconv.Itoa(tooLong.RetryAfter))
		w.Header().Set("X-Quotad", "budget-exhausted")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"message":"quotad: rate-limit budget exhausted; retry later"}` + "\n"))
		return
	case err != nil:
		p.n.gaveUp.Add(1)
		return // the client gave up while held
	}
	p.n.forwarded.Add(1)
	ctx := r.Context()
	if p.rdb != nil {
		// End the request before its lease does, so another copy never
		// counts it as failed while it may still be running. The lease
		// started a moment earlier, by Redis's clock, hence the margin.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.cfg.Lease*9/10)
		defer cancel()
	}
	p.rp.ServeHTTP(w, r.WithContext(context.WithValue(ctx, ticketKey{}, held{t, res})))
}
