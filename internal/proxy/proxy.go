// Package proxy is quotad's HTTP layer: it holds each request until its
// token's limits allow it, forwards it to the upstream, and feeds the
// response back into the budget.
package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
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
			ticket(resp.Request.Context()).Done(resp.StatusCode, resp.Header)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// No usable answer: the upstream may still have counted the request.
			ticket(r.Context()).Failed()
			if r.Context().Err() == nil {
				w.WriteHeader(http.StatusBadGateway)
			}
		},
	}
	return p
}

func ticket(ctx context.Context) *budget.Ticket {
	return ctx.Value(ticketKey{}).(*budget.Ticket)
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
	t, err := p.budgetFor(r.Header.Get("Authorization")).Acquire(r.Context(), write)
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
	p.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ticketKey{}, t)))
}
