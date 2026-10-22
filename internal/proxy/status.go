package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/raghavs6/quotad/internal/budget"
)

// StatusPrefix is the path prefix quotad answers itself instead of
// forwarding. GitHub's API has no paths under it.
const StatusPrefix = "/__quotad/"

// counters are proxy-wide request counts since start.
type counters struct {
	requests   atomic.Int64 // every request to forward
	forwarded  atomic.Int64 // sent to the upstream
	turnedAway atomic.Int64 // answered 429 by quotad: the wait would exceed max hold
	gaveUp     atomic.Int64 // the client left while held
}

// Stats is the body of /__quotad/stats.
type Stats struct {
	Version    string     `json:"version"`
	Upstream   string     `json:"upstream"`
	Requests   int64      `json:"requests"`
	Forwarded  int64      `json:"forwarded"`
	TurnedAway int64      `json:"turned_away"`
	GaveUp     int64      `json:"gave_up"`
	Keys       []KeyStats `json:"keys"`
}

// KeyStats is one token's budget. The token itself is never shown, only a
// short hash of its Authorization header, so people can tell keys apart.
type KeyStats struct {
	Key string `json:"key"`
	budget.Snapshot
}

// keyID is a short, stable, one-way name for an Authorization header.
func keyID(auth string) string {
	if auth == "" {
		return "none"
	}
	sum := sha256.Sum256([]byte(auth))
	return hex.EncodeToString(sum[:4])
}

// serveStatus answers quotad's own paths: health and stats.
func (p *Proxy) serveStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch strings.TrimPrefix(r.URL.Path, StatusPrefix) {
	case "health":
		writeJSON(w, map[string]string{"status": "ok", "version": p.Version})
	case "stats":
		writeJSON(w, p.Stats())
	default:
		http.NotFound(w, r)
	}
}

// Stats returns the proxy's counts and every token's budget.
func (p *Proxy) Stats() Stats {
	type named struct {
		id string
		b  *budget.Budget
	}
	p.mu.Lock()
	budgets := make([]named, 0, len(p.budgets))
	for auth, b := range p.budgets {
		budgets = append(budgets, named{keyID(auth), b})
	}
	p.mu.Unlock()
	keys := make([]KeyStats, 0, len(budgets))
	for _, nb := range budgets {
		keys = append(keys, KeyStats{Key: nb.id, Snapshot: nb.b.Snapshot()})
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Key < keys[j].Key })
	return Stats{
		Version:    p.Version,
		Upstream:   p.upstream.String(),
		Requests:   p.n.requests.Load(),
		Forwarded:  p.n.forwarded.Load(),
		TurnedAway: p.n.turnedAway.Load(),
		GaveUp:     p.n.gaveUp.Load(),
		Keys:       keys,
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
