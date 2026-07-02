package main

import (
	"testing"

	"github.com/fixed-labs/oss/cli/internal/session"
)

func si(id, name string) session.SessionInfo {
	return session.SessionInfo{ID: id, Name: name}
}

// TestDecideSessionDefaultSingle covers the 0/1/>1 default-session selection policy.
func TestDecideSessionDefaultSingle(t *testing.T) {
	t.Run("zero sessions → create", func(t *testing.T) {
		d := decideSession(&session.ListResult{GenEpoch: 1}, connectOpts{}, 0, false)
		if d.sessionID != "" || d.needsPicker {
			t.Fatalf("0 sessions: want create (id=\"\", no picker), got %+v", d)
		}
	})
	t.Run("one session → attach it", func(t *testing.T) {
		d := decideSession(&session.ListResult{GenEpoch: 1, Sessions: []session.SessionInfo{si("s1", "main")}}, connectOpts{}, 0, false)
		if d.sessionID != "s1" || d.needsPicker {
			t.Fatalf("1 session: want attach s1, got %+v", d)
		}
	})
	t.Run("many sessions → picker", func(t *testing.T) {
		list := &session.ListResult{GenEpoch: 1, Sessions: []session.SessionInfo{si("s1", "main"), si("s2", "work")}}
		d := decideSession(list, connectOpts{}, 0, false)
		if !d.needsPicker || len(d.candidates) != 2 {
			t.Fatalf(">1 sessions: want picker over 2, got %+v", d)
		}
		if d.sessionID != "" {
			t.Fatalf(">1 sessions: sessionID must be empty until the picker runs, got %q", d.sessionID)
		}
	})
}

// TestDecideSessionExplicitName attaches by name when present, else creates.
func TestDecideSessionExplicitName(t *testing.T) {
	list := &session.ListResult{Sessions: []session.SessionInfo{si("s1", "main"), si("s2", "work")}}
	d := decideSession(list, connectOpts{sessionName: "work"}, 0, false)
	if d.sessionID != "s2" || d.needsPicker {
		t.Fatalf("--session work: want attach s2, got %+v", d)
	}
	d = decideSession(list, connectOpts{sessionName: "absent"}, 0, false)
	if d.sessionID != "" || d.needsPicker {
		t.Fatalf("--session absent: want create (id=\"\"), got %+v", d)
	}
}

// TestDecideSessionLossNotice fires only when a recorded epoch is exceeded.
func TestDecideSessionLossNotice(t *testing.T) {
	cases := []struct {
		name     string
		genEpoch int64
		prev     int64
		havePrev bool
		want     bool
	}{
		{"no prior record → no notice", 5, 0, false, false},
		{"epoch advanced → notice", 6, 5, true, true},
		{"epoch unchanged → no notice", 5, 5, true, false},
		{"epoch lower (impossible, but guard) → no notice", 4, 5, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := decideSession(&session.ListResult{GenEpoch: c.genEpoch}, connectOpts{}, c.prev, c.havePrev)
			if d.lossNotice != c.want {
				t.Fatalf("lossNotice = %v, want %v", d.lossNotice, c.want)
			}
		})
	}
}
