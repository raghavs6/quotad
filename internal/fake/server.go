package fake

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Server answers like GitHub's REST API, enforcing the limiter's rules.
// Any path works; GET, HEAD and OPTIONS are reads, everything else a write.
// Paths under /search/ count against the search resource, if configured.
// GET /__stats returns every token's counters as JSON.
type Server struct {
	limiter *Limiter
	latency time.Duration // how long an admitted request takes, like real service time
}

// NewServer returns a server. latency is how long each admitted request is
// held before answering; it is what makes the concurrency limit reachable.
func NewServer(l *Limiter, latency time.Duration) *Server {
	return &Server{limiter: l, latency: latency}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/__stats" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.limiter.Stats())
		return
	}

	write := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
	d, release := s.limiter.Begin(r.Header.Get("Authorization"), s.limiter.Resource(r.URL.Path), write)

	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("x-ratelimit-limit", strconv.Itoa(d.Limit))
	h.Set("x-ratelimit-remaining", strconv.Itoa(d.Remaining))
	h.Set("x-ratelimit-used", strconv.Itoa(d.Used))
	h.Set("x-ratelimit-reset", strconv.FormatInt(d.Reset.Unix(), 10))
	h.Set("x-ratelimit-resource", d.Resource)

	switch d.Status {
	case http.StatusOK:
		defer release()
		select {
		case <-time.After(s.latency):
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}` + "\n"))
	case http.StatusForbidden:
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"API rate limit exceeded"}` + "\n"))
	default:
		h.Set("retry-after", strconv.Itoa(d.RetryAfter))
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}` + "\n"))
	}
}
