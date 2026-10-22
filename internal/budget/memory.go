package budget

import (
	"slices"
	"time"
)

// memory keeps one token's counts in this process. It is the default, and
// all a single quotad needs.
type memory struct {
	cfg Config
	now func() time.Time

	resources map[string]*resource
	inFlight  int      // admitted requests not yet answered, across all resources
	spent     []*spend // points of requests in flight or answered within the last PointsWindow
	paused    time.Time
	streak    int // secondary-limit answers in a row without retry-after
}

// resource is one primary budget on the token.
type resource struct {
	known     bool      // we have seen at least one response's headers
	probing   bool      // the one request sent to learn the budget is in flight
	limit     int       // x-ratelimit-limit
	remaining int       // x-ratelimit-remaining, lowest seen this window
	reset     time.Time // x-ratelimit-reset; zero after we roll into a window we haven't seen yet
	reserved  int       // admitted requests not yet answered
}

// spend is one request's points. They count from when it is sent until
// PointsWindow after its answer. The upstream counted it at some moment in
// between, and we can't know when, so the answer time is the safe end.
type spend struct {
	cost     int
	answered time.Time // zero while in flight
}

// close marks the request answered (or abandoned) now.
func (s *spend) close(now time.Time) {
	if s.answered.IsZero() {
		s.answered = now
	}
}

// memLease is memory's lease: the resource it was admitted against, and
// its points.
type memLease struct {
	r  *resource
	sp *spend
}

func newMemory(cfg Config, now func() time.Time) *memory {
	return &memory{cfg: cfg, now: now, resources: map[string]*resource{}}
}

// resource returns the named resource, creating it on first use.
func (m *memory) resource(name string) *resource {
	r := m.resources[name]
	if r == nil {
		r = &resource{}
		m.resources[name] = r
	}
	return r
}

func (m *memory) admit(wants []want) grant {
	now := m.now()
	for _, r := range m.resources {
		r.roll(now, m.cfg.ResetGuard)
	}
	m.prune(now)
	g := grant{leases: make([]lease, len(wants)), release: map[string]time.Duration{}}
	blocked := map[string]bool{}
	stopped := false
	for i, w := range wants {
		r := m.resource(w.res)
		switch {
		case stopped || blocked[w.res]:
		case !r.primaryReady():
			blocked[w.res] = true
		case !m.sharedReady(now, w.cost):
			stopped = true
		default:
			g.leases[i] = m.admitOne(r, w.cost)
		}
	}

	// What the wants left over wait for, and the next moment a check can
	// start passing on its own: a held resource's primary reset, the end of
	// a pause, or the oldest points leaving the window. (Local answers wake
	// the lines themselves.)
	var wake time.Time
	consider := func(t time.Time) {
		if t.After(now) && (wake.IsZero() || t.Before(wake)) {
			wake = t
		}
	}
	held := false
	for i, w := range wants {
		if g.leases[i] != nil {
			continue
		}
		held = true
		r := m.resources[w.res]
		release := m.paused
		if r.primaryOut() && !r.reset.IsZero() {
			release = laterOf(release, r.reset.Add(m.cfg.ResetGuard))
		}
		g.release[w.res] = release.Sub(now)
		if r.known && !r.reset.IsZero() {
			consider(r.reset.Add(m.cfg.ResetGuard))
		}
	}
	if held {
		consider(m.paused)
		for _, s := range m.spent {
			if !s.answered.IsZero() {
				consider(s.answered.Add(m.cfg.PointsWindow))
			}
		}
	}
	if !wake.IsZero() {
		g.wake = wake.Sub(now)
	}
	return g
}

// primaryReady reports whether r's primary budget lets one more request go
// now: as the probe if r is unknown, or because a unit is left.
func (r *resource) primaryReady() bool {
	if !r.known {
		return !r.probing
	}
	return !r.primaryOut()
}

// primaryOut reports whether the primary budget is used up until the
// reset: every remaining unit is already reserved.
func (r *resource) primaryOut() bool {
	return r.known && r.remaining-r.reserved <= 0
}

// sharedReady reports whether the token's shared checks (pause, in-flight
// cap, points) let a request of this cost go now. The caller has pruned.
func (m *memory) sharedReady(now time.Time, cost int) bool {
	return !now.Before(m.paused) && m.inFlight < m.cfg.MaxInFlight && m.points()+cost <= m.cfg.Points
}

// admitOne reserves one unit of r, one in-flight slot and cost points.
func (m *memory) admitOne(r *resource, cost int) *memLease {
	if !r.known {
		r.probing = true // learn first: the rest wait for this one's headers
	}
	r.reserved++
	m.inFlight++
	sp := &spend{cost: cost}
	m.spent = append(m.spent, sp)
	return &memLease{r, sp}
}

// prune forgets points of requests answered more than PointsWindow ago.
func (m *memory) prune(now time.Time) {
	m.spent = slices.DeleteFunc(m.spent, func(s *spend) bool {
		return !s.answered.IsZero() && !s.answered.Add(m.cfg.PointsWindow).After(now)
	})
}

func (m *memory) points() int {
	n := 0
	for _, s := range m.spent {
		n += s.cost
	}
	return n
}

// roll starts a new window once the old one's reset (plus the guard) has
// passed. We assume the full limit until a response says otherwise.
func (r *resource) roll(now time.Time, guard time.Duration) {
	if r.known && !r.reset.IsZero() && !now.Before(r.reset.Add(guard)) {
		r.remaining = r.limit
		r.reset = time.Time{}
	}
}

func (m *memory) answer(l lease, a answer) {
	ml := l.(*memLease)
	now := m.now()
	switch {
	case a.pause > 0:
		m.paused = laterOf(m.paused, now.Add(a.pause))
	case a.backoff:
		m.paused = laterOf(m.paused, now.Add(m.cfg.MinPause<<min(m.streak, 10)))
		m.streak++
	}
	ml.sp.close(now)
	ml.r.reserved--
	m.inFlight--
	if !a.ok {
		// The upstream may still have counted it, so the unit stays spent
		// until a response corrects us.
		if ml.r.known {
			ml.r.remaining--
		} else {
			ml.r.probing = false // let the next request try to learn the budget
		}
		return
	}
	ml.r.probing = false
	if a.status < 300 {
		m.streak = 0
	}
	r := ml.r
	if a.resource != "" {
		r = m.resource(a.resource)
	}
	r.known = true
	r.limit = a.limit
	switch {
	case r.reset.IsZero() || a.reset.After(r.reset):
		// A window we haven't seen yet: take its count as is.
		r.reset = a.reset
		r.remaining = a.remaining
	case a.reset.Equal(r.reset):
		r.remaining = min(r.remaining, a.remaining)
	default:
		// A late answer from an older window says nothing about this one.
	}
}

func (m *memory) cancel(l lease) {
	ml := l.(*memLease)
	ml.sp.close(m.now()) // never sent; its points simply expire
	ml.r.reserved--
	m.inFlight--
	if !ml.r.known {
		ml.r.probing = false
	}
}
