package main

import (
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Sessions: the two-reply flow (.sa on a visual, then .sa on an audio)
// ---------------------------------------------------------------------------

type session struct {
	visual  *mediaRef
	audio   *mediaRef
	started time.Time
}

type putResult struct {
	ready   bool
	visual  mediaRef
	audio   mediaRef
	missing string // what the user still has to send when !ready
}

// Sessions is a TTL map keyed by "chat/sender". Safe for concurrent use.
type Sessions struct {
	mu  sync.Mutex
	m   map[string]*session
	ttl time.Duration
	max int
}

func newSessions(ttl time.Duration, max int) *Sessions {
	return &Sessions{m: make(map[string]*session), ttl: ttl, max: max}
}

// Put stores ref in its slot. When both slots are filled the session is removed and
// returned with ready=true.
func (s *Sessions) Put(key string, ref mediaRef) putResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess := s.m[key]
	if sess != nil && time.Since(sess.started) > s.ttl {
		delete(s.m, key)
		sess = nil
	}
	if sess == nil {
		if len(s.m) >= s.max {
			s.evictOldestLocked()
		}
		sess = &session{started: time.Now()}
		s.m[key] = sess
	}

	r := ref
	if ref.kind.isVisual() {
		sess.visual = &r
	} else {
		sess.audio = &r
	}

	switch {
	case sess.visual != nil && sess.audio != nil:
		res := putResult{ready: true, visual: *sess.visual, audio: *sess.audio}
		delete(s.m, key)
		return res
	case sess.visual != nil:
		return putResult{missing: "audio"}
	default:
		return putResult{missing: "video/GIF"}
	}
}

// HasAudio reports whether the user has a live session that already holds an audio.
func (s *Sessions) HasAudio(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.m[key]
	return sess != nil && sess.audio != nil && time.Since(sess.started) <= s.ttl
}

func (s *Sessions) Reset(key string) {
	s.mu.Lock()
	delete(s.m, key)
	s.mu.Unlock()
}

// Sweep drops expired sessions.
func (s *Sessions) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.m {
		if time.Since(v.started) > s.ttl {
			delete(s.m, k)
		}
	}
}

func (s *Sessions) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

func (s *Sessions) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, v := range s.m {
		if oldestKey == "" || v.started.Before(oldest) {
			oldestKey, oldest = k, v.started
		}
	}
	if oldestKey != "" {
		delete(s.m, oldestKey)
	}
}

// ---------------------------------------------------------------------------
// Guard: one heavy job per user at a time + a per-minute rate limit
// ---------------------------------------------------------------------------

const (
	denyNone = ""
	denyBusy = "busy"
	denyRate = "rate"
)

type Guard struct {
	mu     sync.Mutex
	busy   map[string]bool
	hits   map[string][]time.Time
	perMin int
}

func newGuard(perMin int) *Guard {
	return &Guard{busy: make(map[string]bool), hits: make(map[string][]time.Time), perMin: perMin}
}

// Acquire reserves the user's single job slot. On success it returns a release func
// (safe to call more than once) and denyNone.
func (g *Guard) Acquire(key string) (release func(), deny string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.busy[key] {
		return nil, denyBusy
	}
	now := time.Now()
	recent := g.hits[key][:0]
	for _, t := range g.hits[key] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= g.perMin {
		g.hits[key] = recent
		return nil, denyRate
	}
	g.hits[key] = append(recent, now)
	g.busy[key] = true

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			delete(g.busy, key)
			g.mu.Unlock()
		})
	}, denyNone
}

func (g *Guard) Sweep() {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	for k, ts := range g.hits {
		keep := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < time.Minute {
				keep = append(keep, t)
			}
		}
		if len(keep) == 0 && !g.busy[k] {
			delete(g.hits, k)
		} else {
			g.hits[k] = keep
		}
	}
}

// ---------------------------------------------------------------------------
// Dedupe: WhatsApp can redeliver a message, never run a command twice
// ---------------------------------------------------------------------------

type Dedupe struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
}

func newDedupe(ttl time.Duration) *Dedupe {
	return &Dedupe{seen: make(map[string]time.Time), ttl: ttl}
}

// First returns true the first time id is seen within the TTL.
func (d *Dedupe) First(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.seen[id]; ok && time.Since(t) < d.ttl {
		return false
	}
	d.seen[id] = time.Now()
	return true
}

func (d *Dedupe) Sweep() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, t := range d.seen {
		if time.Since(t) >= d.ttl {
			delete(d.seen, k)
		}
	}
}
