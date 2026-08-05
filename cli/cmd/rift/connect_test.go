package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixed-labs/oss/cli/internal/client"
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

// ---------------------------------------------------------------------------
// waitRunning — the auto-start membership (FIX-313, park ladder gap 2).
//
// A parked box is "suspended" on the warm tier and "stopped" on the cold one,
// and `rift connect` must un-park EITHER. The membership is an inline `||` in
// waitRunning with no named predicate, so the only way to pin it is through
// waitRunning's observable behavior: did a POST …/start get recorded?
//
// The symptom of a revert to `ws.Status == "stopped"` alone is not a crash —
// it is `rift connect` on a warm-parked box silently long-polling a box that
// nobody ever started, until the 5-minute deadline turns into
// "workspace … did not reach running (last: suspended)".
// ---------------------------------------------------------------------------

// waitRunningServer scripts GET /api/workspaces/{id}: the Nth GET answers with
// statuses[N] (the last entry repeating once exhausted), so a table row reads
// as "the box is X, then it becomes running". POSTs answer {"ok":true}.
//
// Every request is recorded as "METHOD <RequestURI>" — RequestURI, not Path,
// because the query string is where the long-poll cursor lives and Path drops
// it. Recording only the path leaves the cursor protocol entirely unpinned: an
// implementation that sent `cursor=` on every iteration would turn each 40 s
// hold into an immediate snapshot read — a hot spin against the API — and
// still satisfy every start-count assertion in this file.
//
// The start is asserted from this log rather than from a stub, so a start that
// goes to the wrong route fails too.
//
// Once the scripted statuses are exhausted the handler HOLDS the GET until the
// request context is cancelled, the way the real long-poll does. Without the
// hold, a caller that never reaches its wanted status spins the server as fast
// as the loopback allows.
func waitRunningServer(id string, statuses []string) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var seen []string
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		if r.Method == http.MethodPost {
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		n := gets
		gets++
		mu.Unlock()
		if n >= len(statuses) {
			// Exhausted: hold like the real long-poll rather than hot-spinning.
			<-r.Context().Done()
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workspace": map[string]any{"workspace_id": id, "status": statuses[n]},
			"cursor":    fmt.Sprintf("c%d", n+1),
		})
	}))
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(seen))
		copy(out, seen)
		return out
	}
}

// checkCursorProtocol asserts the long-poll contract over a recorded request
// log: the FIRST GET carries no cursor (it is the snapshot read), and every
// later GET carries the cursor the previous response issued. waitRunningServer
// hands out "c1", "c2", … in GET order, so the expected cursor on GET n is
// "c<n-1>".
func checkCursorProtocol(t *testing.T, recorded []string) {
	t.Helper()
	getN := 0
	for _, r := range recorded {
		if !strings.HasPrefix(r, "GET ") {
			continue
		}
		getN++
		q := ""
		if i := strings.Index(r, "?"); i >= 0 {
			q = r[i+1:]
		}
		want := ""
		if getN > 1 {
			want = fmt.Sprintf("cursor=c%d", getN-1)
		}
		if q != want {
			t.Fatalf("GET #%d: query = %q, want %q (recorded %v)", getN, q, want, recorded)
		}
	}
}

func TestWaitRunningAutoStartsFromBothRestStates(t *testing.T) {
	const id = "ws-1"
	startRoute := "POST /api/workspaces/" + id + "/start"

	// Every status from which "…then it becomes running" is a sequence the
	// module can actually produce, so the membership is pinned by exclusion as
	// well as inclusion: adding a status here to the auto-start set, or
	// dropping one from it, fails a row.
	//
	// The winding-down transients (stopping/suspending) and `ending` are NOT
	// rows here, because `X → running` is not producible for them — a park
	// transient always settles into a rest state first, and `ending` only ever
	// goes on to `done`. Scripting an impossible transition would still pin the
	// membership, but it would pin it against a fixture that lies about what
	// happens next. They get their own deftest below, against the real settle.
	cases := []struct {
		name      string
		first     string
		wantStart bool
	}{
		{"warm tier: suspended auto-starts", "suspended", true},
		{"cold tier: stopped auto-starts", "stopped", true},
		{"running needs no start", "running", false},
		{"provisioning is not at rest", "provisioning", false},
		{"provisioned is not at rest", "provisioned", false},
		{"starting is not at rest", "starting", false},
		{"resuming is already coming up", "resuming", false},
		{"resizing is not at rest", "resizing", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, recorded := waitRunningServer(id, []string{tc.first, "running"})
			defer srv.Close()

			ws, err := waitRunning(context.Background(), client.New(srv.URL, "tok"), id)
			if err != nil {
				t.Fatalf("waitRunning(%s): %v", tc.first, err)
			}
			if ws.Status != "running" {
				t.Fatalf("waitRunning(%s): landed on %q, want running", tc.first, ws.Status)
			}

			starts := 0
			for _, r := range recorded() {
				if r == startRoute {
					starts++
				}
			}
			switch {
			case tc.wantStart && starts != 1:
				t.Fatalf("%s: want exactly one %q, got %d (recorded %v)",
					tc.first, startRoute, starts, recorded())
			case !tc.wantStart && starts != 0:
				t.Fatalf("%s: must NOT auto-start, but recorded %d %q (recorded %v)",
					tc.first, starts, startRoute, recorded())
			}
			checkCursorProtocol(t, recorded())
		})
	}
}

// TestWaitRunningDoesNotStartABoxObservedWindingDown pins the three statuses
// the table above cannot script: a box observed mid-park (`stopping`,
// `suspending`) or mid-teardown (`ending`) settles into a REST state before it
// could ever be seen `running`, so "first status X, then running" is not a
// sequence the module produces.
//
// It pins two things at once. The membership: waitRunning must NOT start a box
// out from under a park in flight, because the un-park would race the park's
// own effect. And the CONSEQUENCE, which is a real gap this stack discloses
// rather than closes: waitRunning takes its single auto-start decision from the
// FIRST read only (connect.go — the `c.Start` sits before the poll loop, not
// inside it), so a box observed mid-park settles to stopped/suspended and is
// never started. `rift connect` then polls to its 5-minute deadline and exits
// "workspace … did not reach running (last: suspended)".
//
// The park ladder makes "observed mid-park" the common case, so this matters
// more after this stack than before it. Plan §15 considered moving the
// auto-start inside the loop behind a once-per-invocation latch and explicitly
// DELETED both, so the behavior below is intended for now; this deftest is what
// makes the gap visible rather than folklore. Closing it is follow-up work —
// and this deftest is where the fix would flip to `wantStart: true`.
func TestWaitRunningDoesNotStartABoxObservedWindingDown(t *testing.T) {
	const id = "ws-1"
	startRoute := "POST /api/workspaces/" + id + "/start"

	for _, tc := range []struct {
		name     string
		script   []string // the REAL settle, not an invented transition
		wantErr  string
		wantGets int
	}{
		// Settles into a rest state and stays there. waitRunning holds on the
		// long-poll until the caller's context expires — the deadline branch
		// proper is a 5-minute wait, which is the same exit one timeout earlier.
		{"stopping settles cold and is never started", []string{"stopping", "stopped"}, "context deadline exceeded", 3},
		{"suspending settles warm and is never started", []string{"suspending", "suspended"}, "context deadline exceeded", 3},
		// `ending` goes on to `done`, which waitRunning's terminal switch names.
		{"ending is torn down, not started", []string{"ending", "done"}, "is done", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, recorded := waitRunningServer(id, tc.script)
			defer srv.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
			defer cancel()
			_, err := waitRunning(ctx, client.New(srv.URL, "tok"), id)
			if err == nil {
				t.Fatalf("%s: want an error, got success (recorded %v)", tc.name, err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("%s: error = %q, want it to contain %q", tc.name, err, tc.wantErr)
			}

			rec := recorded()
			for _, r := range rec {
				if r == startRoute {
					t.Fatalf("%s: must NOT start a box winding down (recorded %v)", tc.name, rec)
				}
			}
			// Exactly the scripted reads plus (for the two settles) the held one
			// that the context cancels — i.e. it long-polls rather than spinning.
			if len(rec) != tc.wantGets {
				t.Fatalf("%s: %d requests, want %d (recorded %v)", tc.name, len(rec), tc.wantGets, rec)
			}
			checkCursorProtocol(t, rec)
		})
	}
}

// TestWaitRunningTerminalStatusesError pins the other exit: a box that is
// failed/done/destroying is reported, not polled to the 5-minute deadline.
// Without it, a mistake that folded a terminal status into the rest-state set
// would auto-start a destroyed box and then hang.
func TestWaitRunningTerminalStatusesError(t *testing.T) {
	const id = "ws-1"
	for _, st := range []string{"failed", "done", "destroying"} {
		t.Run(st, func(t *testing.T) {
			srv, recorded := waitRunningServer(id, []string{st})
			defer srv.Close()

			_, err := waitRunning(context.Background(), client.New(srv.URL, "tok"), id)
			if err == nil {
				t.Fatalf("waitRunning(%s): want an error, got success", st)
			}
			if !strings.Contains(err.Error(), st) {
				t.Fatalf("waitRunning(%s): error = %q, want it to name the status", st, err)
			}
			for _, r := range recorded() {
				if strings.HasSuffix(r, "/start") {
					t.Fatalf("waitRunning(%s): must not start a terminal box (recorded %v)", st, recorded())
				}
			}
		})
	}
}
