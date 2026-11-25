package main

import (
	"sync"
	"testing"
	"time"
)

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in, arg string
		ok      bool
	}{
		{".sa", "", true},
		{"  .SA  ", "", true},
		{".sa reset", "reset", true},
		{".sax", "", false},
		{"hello .sa", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		arg, ok := parseCommand(".sa", c.in)
		if arg != c.arg || ok != c.ok {
			t.Errorf("parseCommand(%q) = (%q, %v), want (%q, %v)", c.in, arg, ok, c.arg, c.ok)
		}
	}
}

func TestSessionsMerge(t *testing.T) {
	s := newSessions(time.Minute, 10)

	if r := s.Put("u", mediaRef{kind: kindImage}); r.ready || r.missing != "audio" {
		t.Fatalf("visual first: %+v", r)
	}
	r := s.Put("u", mediaRef{kind: kindAudio})
	if !r.ready || r.visual.kind != kindImage || r.audio.kind != kindAudio {
		t.Fatalf("audio second: %+v", r)
	}
	if s.Len() != 0 {
		t.Fatal("session should be removed after merge")
	}
}

func TestSessionsAudioFirstAndReset(t *testing.T) {
	s := newSessions(time.Minute, 10)
	if r := s.Put("u", mediaRef{kind: kindAudio}); r.ready || r.missing != "video/GIF" {
		t.Fatalf("audio first: %+v", r)
	}
	if !s.HasAudio("u") {
		t.Fatal("HasAudio should be true")
	}
	s.Reset("u")
	if s.HasAudio("u") {
		t.Fatal("HasAudio should be false after reset")
	}
}

func TestSessionsExpireAndCap(t *testing.T) {
	s := newSessions(10*time.Millisecond, 2)
	s.Put("a", mediaRef{kind: kindImage})
	time.Sleep(20 * time.Millisecond)
	if r := s.Put("a", mediaRef{kind: kindAudio}); r.ready {
		t.Fatal("expired session must not merge")
	}

	s = newSessions(time.Minute, 2)
	s.Put("a", mediaRef{kind: kindImage})
	s.Put("b", mediaRef{kind: kindImage})
	s.Put("c", mediaRef{kind: kindImage})
	if s.Len() != 2 {
		t.Fatalf("cap not enforced: %d", s.Len())
	}
}

func TestGuard(t *testing.T) {
	g := newGuard(2)
	rel, deny := g.Acquire("u")
	if deny != denyNone {
		t.Fatal("first acquire denied")
	}
	if _, deny := g.Acquire("u"); deny != denyBusy {
		t.Fatalf("want busy, got %q", deny)
	}
	rel()
	rel() // idempotent
	rel2, deny := g.Acquire("u")
	if deny != denyNone {
		t.Fatal("second acquire denied")
	}
	rel2()
	if _, deny := g.Acquire("u"); deny != denyRate {
		t.Fatalf("want rate, got %q", deny)
	}
	// Other users are unaffected.
	if _, deny := g.Acquire("other"); deny != denyNone {
		t.Fatal("other user denied")
	}
}

func TestGuardConcurrent(t *testing.T) {
	g := newGuard(1000)
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rel, deny := g.Acquire("same"); deny == denyNone {
				mu.Lock()
				granted++
				mu.Unlock()
				time.Sleep(20 * time.Millisecond)
				rel()
			}
		}()
	}
	wg.Wait()
	if granted < 1 {
		t.Fatal("nobody got the slot")
	}
}

func TestDedupe(t *testing.T) {
	d := newDedupe(time.Minute)
	if !d.First("x") || d.First("x") || !d.First("y") {
		t.Fatal("dedupe misbehaves")
	}
}
