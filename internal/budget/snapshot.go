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
	Waiting   int        `json:"waiting"`  // held in this quotad
	Reset     *time.Time `json:"reset,omitempty"`
}

// Snapshot returns the budget's current state. Resources are sorted by name.
// With Redis, the counts are every copy's, and Waiting is this copy's.
func (b *Budget) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.c.snapshot()
	for res, line := range b.lines {
		i := 0
		for i < len(s.Resources) && s.Resources[i].Name != res {
			i++
		}
		if i == len(s.Resources) {
			s.Resources = append(s.Resources, ResourceSnapshot{Name: res})
		}
		s.Resources[i].Waiting = len(line)
	}
	sort.Slice(s.Resources, func(i, j int) bool { return s.Resources[i].Name < s.Resources[j].Name })
	return s
}

func (m *memory) snapshot() Snapshot {
	now := m.now()
	m.prune(now)
	s := Snapshot{InFlight: m.inFlight, Points: m.points(), Resources: []ResourceSnapshot{}}
	if m.paused.After(now) {
		t := m.paused
		s.PausedUntil = &t
	}
	for name, r := range m.resources {
		rs := ResourceSnapshot{Name: name, Known: r.known, Limit: r.limit, Remaining: r.remaining, Reserved: r.reserved}
		if !r.reset.IsZero() {
			t := r.reset
			rs.Reset = &t
		}
		s.Resources = append(s.Resources, rs)
	}
	return s
}
