// Package fake holds the rate-limit rules of the fake GitHub server.
//
// The rules follow GitHub's REST API, with sizes and windows we choose:
//   - Primary limit: Budget requests per Window, per token. Over it: 403
//     with x-ratelimit-remaining: 0.
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
	"sync"
	"time"
)

// Point costs, from GitHub's secondary-limit rules.
const (
	ReadCost  = 1
	WriteCost = 5
)

// Config sets the limits. Window must be a whole number of seconds so that
// reset times, which GitHub sends as whole epoch seconds, are exact.
type Config struct {
	Budget        int
	Window        time.Duration
	MaxConcurrent int
	Points        int
	PointsWindow  time.Duration
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
	}
	return nil
}

// Decision is the limiter's answer to one request.
type Decision struct {
	Status     int // http.StatusOK, StatusForbidden (primary) or StatusTooManyRequests (secondary)
	Limit      int
	Remaining  int
	Used       int
	Reset      time.Time // when the primary window resets, on a whole second
	RetryAfter int       // seconds; set only on 429
	Violation  bool      // the token was already limited when this request arrived
}

// Stats counts what happened to one token.
type Stats struct {
	Requests     int `json:"requests"`
	OK           int `json:"ok"`
	Primary403   int `json:"primary_403"`
	Secondary429 int `json:"secondary_429"`
	Violations   int `json:"violations"`
	Windows      int `json:"windows"` // primary windows started, for utilization
}

type tokenState struct {
	used     int
	resetAt  time.Time
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

// Begin decides one request. If the request is admitted (Status 200), the
// caller must call the returned release func when the request finishes, so
// it stops counting toward the concurrency limit. release is nil otherwise.
func (l *Limiter) Begin(token string, write bool) (d Decision, release func()) {
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

	// Like GitHub, a window starts with the first request after the last one
	// ended. We start it on a whole second so the reset header is exact.
	if !t.Before(st.resetAt) {
		st.resetAt = t.Truncate(time.Second).Add(l.cfg.Window)
		st.used = 0
		st.stats.Windows++
	}
	if !t.Before(st.pointsAt) {
		st.pointsAt = t.Add(l.cfg.PointsWindow)
		st.points = 0
	}

	st.stats.Requests++
	d.Violation = st.used >= l.cfg.Budget || t.Before(st.penalty)
	if d.Violation {
		st.stats.Violations++
	}

	switch {
	case t.Before(st.penalty):
		d.Status = http.StatusTooManyRequests
		d.RetryAfter = ceilSeconds(st.penalty.Sub(t))
	case st.used >= l.cfg.Budget:
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
		st.used++
		st.points += cost
		st.inflight++
		st.stats.OK++
		release = sync.OnceFunc(func() {
			l.mu.Lock()
			st.inflight--
			l.mu.Unlock()
		})
	}

	switch d.Status {
	case http.StatusForbidden:
		st.stats.Primary403++
	case http.StatusTooManyRequests:
		st.stats.Secondary429++
	}

	d.Limit = l.cfg.Budget
	d.Used = st.used
	d.Remaining = l.cfg.Budget - st.used
	d.Reset = st.resetAt
	return d, release
}

// Stats returns a copy of every token's counters.
func (l *Limiter) Stats() map[string]Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]Stats, len(l.tokens))
	for tok, st := range l.tokens {
		out[tok] = st.stats
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
