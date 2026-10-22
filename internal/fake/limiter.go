// Package fake holds the rate-limit rules of the fake GitHub server.
//
// The rules follow GitHub's REST API, with sizes and windows we choose:
//   - Primary limit: Budget requests per Window, per token. Over it: 403
//     with x-ratelimit-remaining: 0.
//   - Like GitHub, search has its own primary limit: SearchBudget requests
//     per SearchWindow, counted apart from core. With SearchBudget 0, there
//     is no search resource and searches count as core.
//   - Secondary limits: at most MaxConcurrent requests in flight, and at most
//     Points points per PointsWindow (read = 1, write = 5). Over them: 429
//     with retry-after.
//
// Every request that arrives while its token is limited is a violation.
package fake

import (
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Point costs, from GitHub's secondary-limit rules.
const (
	ReadCost  = 1
	WriteCost = 5
)

// Resources, as named in the x-ratelimit-resource header.
const (
	Core   = "core"
	Search = "search"
)

// Config sets the limits. Window and SearchWindow must be whole numbers of
// seconds so that reset times, which GitHub sends as whole epoch seconds,
// are exact.
type Config struct {
	Budget        int
	Window        time.Duration
	MaxConcurrent int
	Points        int
	PointsWindow  time.Duration
	SearchBudget  int // 0 = no search resource
	SearchWindow  time.Duration
}

// Validate reports a config the limiter can't run with.
func (c Config) Validate() error {
	switch {
	case c.Budget <= 0:
		return fmt.Errorf("budget must be positive, got %d", c.Budget)
	case c.Window < time.Second || c.Window%time.Second != 0:
		return fmt.Errorf("window must be a whole number of seconds, got %v", c.Window)
	case c.MaxConcurrent <= 0:
		return fmt.Errorf("max concurrent must be positive, got %d", c.MaxConcurrent)
	case c.Points < WriteCost:
		return fmt.Errorf("points must be at least %d, got %d", WriteCost, c.Points)
	case c.PointsWindow <= 0:
		return fmt.Errorf("points window must be positive, got %v", c.PointsWindow)
	case c.SearchBudget < 0:
		return fmt.Errorf("search budget can't be negative, got %d", c.SearchBudget)
	case c.SearchBudget > 0 && (c.SearchWindow < time.Second || c.SearchWindow%time.Second != 0):
		return fmt.Errorf("search window must be a whole number of seconds, got %v", c.SearchWindow)
	}
	return nil
}

// Decision is the limiter's answer to one request.
type Decision struct {
	Status     int // http.StatusOK, StatusForbidden (primary) or StatusTooManyRequests (secondary)
	Resource   string
	Limit      int
	Remaining  int
	Used       int
	Reset      time.Time // when the primary window resets, on a whole second
	RetryAfter int       // seconds; set only on 429
	Violation  bool      // the token was already limited when this request arrived
}

// Stats counts what happened to one token. The counts cover every
// resource; the window fields are core's.
type Stats struct {
	Requests     int `json:"requests"`
	OK           int `json:"ok"`
	Primary403   int `json:"primary_403"`
	Secondary429 int `json:"secondary_429"`
	Violations   int `json:"violations"`
	Windows      int `json:"windows"` // primary windows started
	// Per window, in order: when it reset (epoch seconds) and how many
	// requests succeeded in it. Used to compute utilization over full windows.
	WindowResets []int64 `json:"window_resets"`
	WindowOK     []int   `json:"window_ok"`

	Search *ResourceStats `json:"search,omitempty"` // only if a search was sent
}

// ResourceStats counts one resource apart from core.
type ResourceStats struct {
	Requests     int     `json:"requests"`
	OK           int     `json:"ok"`
	Primary403   int     `json:"primary_403"`
	Violations   int     `json:"violations"`
	WindowResets []int64 `json:"window_resets"`
	WindowOK     []int   `json:"window_ok"`
}

// primary is one resource's primary window.
type primary struct {
	used    int
	resetAt time.Time
}

type tokenState struct {
	core     primary
	search   primary
	points   int
	pointsAt time.Time // when the points window resets
	inflight int
	penalty  time.Time // retry-after deadline from the last 429
	stats    Stats
}

// Limiter applies the rules. It is safe for concurrent use.
type Limiter struct {
	cfg Config
	now func() time.Time

	mu     sync.Mutex
	tokens map[string]*tokenState
}

// NewLimiter returns a limiter that reads time from now, so tests can
// control the clock.
func NewLimiter(cfg Config, now func() time.Time) (*Limiter, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Limiter{cfg: cfg, now: now, tokens: map[string]*tokenState{}}, nil
}

// Resource names the resource a request to path counts against.
func (l *Limiter) Resource(path string) string {
	if l.cfg.SearchBudget > 0 && strings.HasPrefix(path, "/search/") {
		return Search
	}
	return Core
}

// Begin decides one request to resource (Core, or Search if configured). If
// the request is admitted (Status 200), the caller must call the returned
// release func when the request finishes, so it stops counting toward the
// concurrency limit. release is nil otherwise.
func (l *Limiter) Begin(token, resource string, write bool) (d Decision, release func()) {
	cost := ReadCost
	if write {
		cost = WriteCost
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	t := l.now()
	st := l.tokens[token]
	if st == nil {
		st = &tokenState{}
		l.tokens[token] = st
	}

	// The resource's primary window, and where its per-window counts go.
	p, budget, window := &st.core, l.cfg.Budget, l.cfg.Window
	resets, oks := &st.stats.WindowResets, &st.stats.WindowOK
	var rs *ResourceStats
	if resource == Search {
		if st.stats.Search == nil {
			st.stats.Search = &ResourceStats{}
		}
		rs = st.stats.Search
		p, budget, window = &st.search, l.cfg.SearchBudget, l.cfg.SearchWindow
		resets, oks = &rs.WindowResets, &rs.WindowOK
	}

	// Like GitHub, a window starts with the first request after the last one
	// ended. We start it on a whole second so the reset header is exact.
	if !t.Before(p.resetAt) {
		p.resetAt = t.Truncate(time.Second).Add(window)
		p.used = 0
		if rs == nil {
			st.stats.Windows++
		}
		*resets = append(*resets, p.resetAt.Unix())
		*oks = append(*oks, 0)
	}
	if !t.Before(st.pointsAt) {
		st.pointsAt = t.Add(l.cfg.PointsWindow)
		st.points = 0
	}

	st.stats.Requests++
	d.Violation = p.used >= budget || t.Before(st.penalty)
	if d.Violation {
		st.stats.Violations++
	}
	if rs != nil {
		rs.Requests++
		if d.Violation {
			rs.Violations++
		}
	}

	switch {
	case t.Before(st.penalty):
		d.Status = http.StatusTooManyRequests
		d.RetryAfter = ceilSeconds(st.penalty.Sub(t))
	case p.used >= budget:
		d.Status = http.StatusForbidden
	case st.inflight >= l.cfg.MaxConcurrent:
		d.Status = http.StatusTooManyRequests
		d.RetryAfter = 1
		st.penalize(t, d.RetryAfter)
	case st.points+cost > l.cfg.Points:
		d.Status = http.StatusTooManyRequests
		d.RetryAfter = ceilSeconds(st.pointsAt.Sub(t))
		st.penalize(t, d.RetryAfter)
	default:
		d.Status = http.StatusOK
		p.used++
		st.points += cost
		st.inflight++
		st.stats.OK++
		(*oks)[len(*oks)-1]++
		if rs != nil {
			rs.OK++
		}
		release = sync.OnceFunc(func() {
			l.mu.Lock()
			st.inflight--
			l.mu.Unlock()
		})
	}

	switch d.Status {
	case http.StatusForbidden:
		st.stats.Primary403++
		if rs != nil {
			rs.Primary403++
		}
	case http.StatusTooManyRequests:
		st.stats.Secondary429++
	}

	d.Resource = resource
	d.Limit = budget
	d.Used = p.used
	d.Remaining = budget - p.used
	d.Reset = p.resetAt
	return d, release
}

// Stats returns a copy of every token's counters.
func (l *Limiter) Stats() map[string]Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]Stats, len(l.tokens))
	for tok, st := range l.tokens {
		s := st.stats
		s.WindowResets = slices.Clone(s.WindowResets)
		s.WindowOK = slices.Clone(s.WindowOK)
		if s.Search != nil {
			rs := *s.Search
			rs.WindowResets = slices.Clone(rs.WindowResets)
			rs.WindowOK = slices.Clone(rs.WindowOK)
			s.Search = &rs
		}
		out[tok] = s
	}
	return out
}

// penalize records a fresh secondary-limit hit. The caller is told to wait
// retryAfter seconds; anything it sends before then is a violation. A 429
// sent during an existing penalty doesn't extend it.
func (st *tokenState) penalize(t time.Time, retryAfter int) {
	st.penalty = t.Add(time.Duration(retryAfter) * time.Second)
}

// ceilSeconds rounds up to whole seconds, at least 1, as retry-after needs.
func ceilSeconds(d time.Duration) int {
	return max(1, int(math.Ceil(d.Seconds())))
}
