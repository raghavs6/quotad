package budget

import (
	"sort"
	"time"
)

// Snapshot is a point-in-time view of one token's budget, for the stats page.
type Snapshot struct {
	InFlight    int                `json:"in_flight"`
	Points      int                `json:"points"` // spent in the current points window
	PausedUntil *time.Time         `json:"paused_until,omitempty"`
	Resources   []ResourceSnapshot `json:"resources"`
}

// ResourceSnapshot is one primary budget on the token.
type ResourceSnapshot struct {
	Name      string     `json:"name"`
	Known     bool       `json:"known"` // false until the upstream's headers have been seen
	Limit     int        `json:"limit"`
	Remaining int        `json:"remaining"`
	Reserved  int        `json:"reserved"` // sent, not yet answered
	Waiting   int        `json:"waiting"`  // held in quotad
	Reset     *time.Time `json:"reset,omitempty"`
}

// Snapshot returns the budget's current state. Resources are sorted by name.
func (b *Budget) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.pruneLocked(now)
	s := Snapshot{InFlight: b.inFlight, Points: b.pointsLocked(), Resources: []ResourceSnapshot{}}
	if b.paused.After(now) {
		t := b.paused
		s.PausedUntil = &t
	}
	for name, r := range b.resources {
		rs := ResourceSnapshot{Name: name, Known: r.known, Limit: r.limit, Remaining: r.remaining, Reserved: r.reserved, Waiting: len(r.waiting)}
		if !r.reset.IsZero() {
			t := r.reset
			rs.Reset = &t
		}
		s.Resources = append(s.Resources, rs)
	}
	sort.Slice(s.Resources, func(i, j int) bool { return s.Resources[i].Name < s.Resources[j].Name })
	return s
}
