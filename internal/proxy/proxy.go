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
)

// Proxy forwards to one upstream, with one budget per token.
type Proxy struct {
	cfg      budget.Config
	now      func() time.Time
	schedule func(time.Duration, func())
	rp       *httputil.ReverseProxy

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

// New returns a proxy to upstream.
func New(upstream *url.URL, cfg budget.Config) *Proxy {
	p := &Proxy{cfg: cfg, now: time.Now, schedule: budget.AfterFunc, budgets: map[string]*budget.Budget{}}
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
		b = budget.New(p.cfg, p.now, p.schedule)
		p.budgets[token] = b
	}
	return b
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Like GitHub's points rule: GET, HEAD and OPTIONS are reads, the rest writes.
	write := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
	res := classifyResource(r.URL.Path)
	t, err := p.budgetFor(r.Header.Get("Authorization")).Acquire(r.Context(), res, write)
	var tooLong *budget.TooLongError
	switch {
	case errors.As(err, &tooLong):
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", strconv.Itoa(tooLong.RetryAfter))
		w.Header().Set("X-Quotad", "budget-exhausted")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"message":"quotad: rate-limit budget exhausted; retry later"}` + "\n"))
		return
	case err != nil:
		return // the client gave up while held
	}
	p.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ticketKey{}, held{t, res})))
}
