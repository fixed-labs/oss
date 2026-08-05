package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixed-labs/oss/cli/internal/client"
)

// ---------------------------------------------------------------------------
// `rift restart` — the client-side compose (FIX-316 / park ladder WS6c).
//
// restart is a COMPOSE, not a server verb: it reads the box's status, resolves
// it to one of the three dispatchable states (polling first when it is a park
// transient or still coming up), then issues `stop --cold` and/or `start`,
// polling each leg to an exact target. The design's dispatch table is total
// over all thirteen statuses a row can hold, and every one of the four poll
// phases aborts on a teardown.
//
// Every assertion below reads a RECORDED ROUTE LOG rather than a call count:
// the failure modes this pins differ only in WHICH route is hit and WHICH
// command string is printed, so "an error occurred" or "one POST happened"
// would pass under most of the wrong branches.
// ---------------------------------------------------------------------------

// cpReq is one request the fake control plane served.
type cpReq struct {
	Route  string // "METHOD /path" — path ONLY, so the route assertions stay stable
	Body   string // request body (POSTs)
	Served string // the status this GET answered with
	Cursor string // the ?cursor= this GET carried ("" = snapshot read)
}

const (
	// cpNotModified is a GET-script sentinel: answer that long-poll 304
	// (hold expired, nothing changed) instead of a workspace.
	cpNotModified = "<304>"
	// cpExhausted is recorded when the compose asks for more GETs than the
	// script has. The fake then 500s, so an off-script poll fails the test in
	// milliseconds instead of spinning to restartPoll's 5-minute deadline.
	cpExhausted = "<script exhausted>"
	// cpIneligible is the ONE 409 the compose retries: the row was
	// mid-transition when the call landed.
	cpIneligible = `{"error":"ineligible-status","detail":"workspace status: stopping"}`
)

// restartCP is a scripted fake control plane. statuses answers successive GETs
// in order — so a row reads as an exact request sequence, and an extra or
// missing poll hop is a failure, not a silent pass. conflicts[leaf] answers
// that many leading POSTs to the leaf with conflictBody at 409.
//
// stick changes what happens once the script runs out. By default the fake 500s
// (cpExhausted), so an off-script poll fails in milliseconds instead of spinning
// to a phase deadline. With stick set, the LAST scripted status repeats forever
// — a box that simply never moves, which is the only way to reach the deadline
// give-up branch. See TestRestartPhaseDeadlineGiveUpCopy.
type restartCP struct {
	id           string
	client       *client.Client
	conflicts    map[string]int
	conflictBody string
	stick        bool

	mu       sync.Mutex
	statuses []string
	gets     int
	reqs     []cpReq
}

func newRestartCP(t *testing.T, id string, statuses []string) *restartCP {
	t.Helper()
	cp := &restartCP{
		id:           id,
		statuses:     statuses,
		conflicts:    map[string]int{},
		conflictBody: cpIneligible,
	}
	srv := httptest.NewServer(cp)
	t.Cleanup(srv.Close)
	cp.client = client.New(srv.URL, "tok")
	return cp
}

// newStuckCP is a fake whose box reaches statuses' last entry and STAYS there,
// so the poll phase under test runs to its deadline instead of resolving.
// Pair it with shortRestartDeadlines.
func newStuckCP(t *testing.T, id string, statuses []string) *restartCP {
	t.Helper()
	cp := newRestartCP(t, id, statuses)
	cp.stick = true
	return cp
}

// shortRestartDeadlines shrinks the phase deadline and the long-poll hold to
// milliseconds for the duration of one test, restoring them afterwards. The
// production values are five minutes and forty seconds; without this the
// deadline branch cannot be reached by any test at all.
func shortRestartDeadlines(t *testing.T) {
	t.Helper()
	oldDeadline, oldHold := restartPhaseDeadline, restartPollHold
	restartPhaseDeadline, restartPollHold = 150*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { restartPhaseDeadline, restartPollHold = oldDeadline, oldHold })
}

func (cp *restartCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	route := r.Method + " " + r.URL.Path
	if r.Method != http.MethodGet {
		body, _ := io.ReadAll(r.Body)
		cp.reqs = append(cp.reqs, cpReq{Route: route, Body: string(body)})
		leaf := path.Base(r.URL.Path)
		if n := cp.conflicts[leaf]; n > 0 {
			cp.conflicts[leaf] = n - 1
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, cp.conflictBody)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
		return
	}
	cursor := r.URL.Query().Get("cursor")
	if cp.gets >= len(cp.statuses) {
		if !cp.stick {
			cp.reqs = append(cp.reqs, cpReq{Route: route, Served: cpExhausted, Cursor: cursor})
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"cp GET script exhausted"}`)
			return
		}
		// STICK: the box has reached its last scripted status and stays there.
		// A cursored read is a long poll, and the faithful answer to "held for
		// the full window, nothing changed" is a 304 — which is what paces the
		// caller's loop to its phase deadline instead of spinning the server.
		// A cursor-less read is a snapshot, where a 304 is a real fault by
		// contract, so that one still answers with the status.
		if cursor != "" {
			cp.gets++
			cp.reqs = append(cp.reqs, cpReq{Route: route, Served: cpNotModified, Cursor: cursor})
			// Answer WELL inside the caller's own hold timeout — the caller
			// bounds each long poll by restartPollHold too, so a server that
			// slept the full window would trip the client's context first and
			// surface a transport error instead of the deadline give-up.
			hold := restartPollHold / 4
			cp.mu.Unlock()
			time.Sleep(hold)
			cp.mu.Lock()
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	st := cp.statuses[min(cp.gets, len(cp.statuses)-1)]
	cp.gets++
	cp.reqs = append(cp.reqs, cpReq{Route: route, Served: st, Cursor: cursor})
	if st == cpNotModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"workspace": map[string]any{"workspace_id": cp.id, "status": st},
		"cursor":    fmt.Sprintf("c%d", cp.gets),
	})
}

func (cp *restartCP) log() []cpReq {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	out := make([]cpReq, len(cp.reqs))
	copy(out, cp.reqs)
	return out
}

// posts is the dispatch log: the routes of every non-GET request, in order.
func (cp *restartCP) posts() []string {
	var out []string
	for _, r := range cp.log() {
		if !strings.HasPrefix(r.Route, http.MethodGet+" ") {
			out = append(out, r.Route)
		}
	}
	return out
}

// bodyOf returns the body of the first request recorded against route.
func (cp *restartCP) bodyOf(route string) (string, bool) {
	for _, r := range cp.log() {
		if r.Route == route {
			return r.Body, true
		}
	}
	return "", false
}

func (cp *restartCP) getsServed() int {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.gets
}

// cursors is every GET's ?cursor=, in order ("" = a snapshot read).
func (cp *restartCP) cursors() []string {
	var out []string
	for _, r := range cp.log() {
		if strings.HasPrefix(r.Route, http.MethodGet+" ") {
			out = append(out, r.Cursor)
		}
	}
	return out
}

// checkCursorChain asserts cursor INTEGRITY across a whole compose run: a GET
// either opens a phase (no cursor — restartStatus and each restartPoll begin
// with a snapshot read) or continues one, in which case it must carry exactly
// the cursor the immediately preceding response issued. A stale or fabricated
// cursor — a phase re-sending a cursor from two hops back, say — fails here.
//
// It deliberately does NOT try to infer phase boundaries, so it cannot on its
// own rule out a loop that sends an empty cursor every iteration. That is what
// TestRestartPollLongPollsWithTheCursor pins, on a single phase where the
// expected sequence is exact.
func (cp *restartCP) checkCursorChain(t *testing.T) {
	t.Helper()
	issued := ""
	served := 0
	for i, r := range cp.log() {
		if !strings.HasPrefix(r.Route, http.MethodGet+" ") {
			continue
		}
		served++
		if r.Cursor != "" && r.Cursor != issued {
			t.Fatalf("GET #%d (log entry %d, served %q): cursor = %q — neither empty (a phase-opening snapshot) nor %q (what the previous response issued)",
				served, i, r.Served, r.Cursor, issued)
		}
		// A 304 issues no new cursor; the client re-polls with the one it holds.
		if r.Served != cpNotModified && r.Served != cpExhausted {
			issued = fmt.Sprintf("c%d", served)
		}
	}
}

func routesEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// runRestart drives the compose with stdout captured (restart prints progress
// lines), returning the printed output and the error.
func runRestart(t *testing.T, cp *restartCP, id string) (string, error) {
	t.Helper()
	return runRestartCtx(t, cp, id, context.Background())
}

// runRestartCtx is runRestart with a caller-supplied context. The deadline tests
// pass a bounded one so that a compose which never gives up FAILS rather than
// HANGS: with the deadline branch deleted, an unbounded run polls a stuck box
// forever and the only signal is the Go test binary's own 10-minute panic, which
// reads as infrastructure trouble rather than a missing guard. Bounded, the same
// mutation surfaces in seconds as "context deadline exceeded" where the give-up
// copy was expected.
func runRestartCtx(t *testing.T, cp *restartCP, id string, ctx context.Context) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() { err = restart(ctx, cp.client, id) })
	// Every compose run is also a cursor-integrity run — asserted here rather than
	// per-deftest so no row can be added that skips it.
	cp.checkCursorChain(t)
	return out, err
}

// --- 6N1: the compose table, total over all thirteen statuses ---------------

// TestRestartComposeTableOverEveryStatus walks the design's dispatch table
// (§4.3 "The cold override on the wire", plan D5) status by status. Three
// dispatch immediately, seven poll to a settled status first, three error.
//
// The pre-ready rows are the ones a table written from the pre-PR-4 world gets
// wrong: the bring-up-deadline park takes a box that never came up from
// `starting`/`provisioned` to `stopped`, and such a box IS restartable — a
// `running`-or-terminal poll set hangs on exactly that cohort. Two rows below
// land pre-ready boxes on `stopped` for that reason.
//
// Each row pins the exact POST sequence AND that the GET script was consumed
// exactly, so a dropped poll hop (dispatching straight out of a transient) or
// an extra one fails even when the landing is right.
func TestRestartComposeTableOverEveryStatus(t *testing.T) {
	const id = "ws-1"
	stopRoute := "POST /api/workspaces/" + id + "/stop"
	startRoute := "POST /api/workspaces/" + id + "/start"

	cases := []struct {
		name string
		// script answers successive GETs: [status read, then each poll read].
		script    []string
		wantPosts []string
		wantErr   []string // substrings the error must carry; nil ⇒ success
	}{
		// --- dispatch immediately ---
		{
			name:      "running: cold stop, poll to stopped, start",
			script:    []string{"running", "stopping", "stopped", "running"},
			wantPosts: []string{stopRoute, startRoute},
		},
		{
			// The cold-DOWN: a box already parked warm still takes the stop leg,
			// because only a stop invalidates the RAM snapshot restart exists to
			// discard.
			name:      "suspended: cold-down, poll to stopped, start",
			script:    []string{"suspended", "stopping", "stopped", "running"},
			wantPosts: []string{stopRoute, startRoute},
		},
		{
			name:      "stopped: start only — nothing to discard",
			script:    []string{"stopped", "running"},
			wantPosts: []string{startRoute},
		},

		// --- the four park/realization transients poll to settled first ---
		{
			name:      "stopping settles to stopped, then start only",
			script:    []string{"stopping", "stopping", "stopped", "running"},
			wantPosts: []string{startRoute},
		},
		{
			// Settles on the OTHER rest state, which still takes the cold-down.
			name:      "suspending settles to suspended, then cold-down + start",
			script:    []string{"suspending", "suspending", "suspended", "stopped", "running"},
			wantPosts: []string{stopRoute, startRoute},
		},
		{
			name:      "resuming settles to running, then cold stop + start",
			script:    []string{"resuming", "resuming", "running", "stopped", "running"},
			wantPosts: []string{stopRoute, startRoute},
		},
		{
			name:      "resizing settles to running, then cold stop + start",
			script:    []string{"resizing", "resizing", "running", "stopped", "running"},
			wantPosts: []string{stopRoute, startRoute},
		},

		// --- the three pre-ready states poll to running, a REST STATE, or terminal ---
		{
			name:      "provisioning comes up running, then cold stop + start",
			script:    []string{"provisioning", "provisioning", "running", "stopped", "running"},
			wantPosts: []string{stopRoute, startRoute},
		},
		{
			// The deadline-park cohort: a pre-ready box that never came up lands
			// `stopped`. Accepting that rest state is what keeps the phase from
			// burning its whole deadline.
			name:      "provisioned deadline-parks to stopped, then start only",
			script:    []string{"provisioned", "provisioned", "stopped", "running"},
			wantPosts: []string{startRoute},
		},
		{
			name:      "starting deadline-parks to stopped, then start only",
			script:    []string{"starting", "starting", "stopped", "running"},
			wantPosts: []string{startRoute},
		},
		{
			name:      "starting comes up running, then cold stop + start",
			script:    []string{"starting", "starting", "running", "stopped", "running"},
			wantPosts: []string{stopRoute, startRoute},
		},

		// --- the three teardown states error, dispatching nothing ---
		{
			name:    "ending errors: a teardown wins over a revival",
			script:  []string{"ending"},
			wantErr: []string{"ending", "torn down"},
		},
		{
			name:    "destroying errors",
			script:  []string{"destroying"},
			wantErr: []string{"destroying", "torn down"},
		},
		{
			name:    "done errors",
			script:  []string{"done"},
			wantErr: []string{"done", "torn down"},
		},

		// --- totality by exclusion: a status outside all four sets must ERROR,
		// not fall through into a dispatch or spin in a poll. ---
		{
			name:    "an unmodelled status errors rather than dispatching",
			script:  []string{"failed"},
			wantErr: []string{"failed", "no move"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := newRestartCP(t, id, tc.script)
			out, err := runRestart(t, cp, id)

			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("restart: %v (recorded %+v)", err, cp.log())
				}
				// A compose that returns nil without reporting completion has
				// left the user unable to tell a restart from a no-op.
				if want := id + ": restarted"; !strings.Contains(out, want) {
					t.Fatalf("stdout = %q, want it to report %q", out, want)
				}
			} else {
				if err == nil {
					t.Fatalf("restart: want an error, got success (recorded %+v)", cp.log())
				}
				for _, sub := range tc.wantErr {
					if !strings.Contains(err.Error(), sub) {
						t.Fatalf("restart error = %q, want it to contain %q", err, sub)
					}
				}
			}

			if got := cp.posts(); !routesEqual(got, tc.wantPosts) {
				t.Fatalf("dispatch log = %v, want %v (full log %+v)", got, tc.wantPosts, cp.log())
			}
			// getsServed counts SCRIPT-CONSUMING GETs. A GET past the end of the
			// script does not increment it (the fake 500s instead), so an
			// over-poll is caught by that 500 propagating out of restart, not by
			// this equality — which is exactly why the rows below also assert the
			// error. Stated because "exactly N GETs" reads stronger than it is.
			if got, want := cp.getsServed(), len(tc.script); got != want {
				t.Fatalf("served %d scripted GETs, want exactly %d — the compose polled a different number of times than the table says (full log %+v)",
					got, want, cp.log())
			}
			// Whenever restart stops a box it stops it COLD. A warm stop here
			// would park the RAM image the restart exists to discard, and the
			// failure is invisible from the CLI's own output.
			if body, ok := cp.bodyOf(stopRoute); ok {
				if body != `{"cold":true}` {
					t.Fatalf("restart's stop leg body = %q, want %q", body, `{"cold":true}`)
				}
			}
		})
	}
}

// --- 6N2: every poll phase aborts on a teardown ----------------------------

// TestRestartPollPhasesAbortOnTeardown drives all FOUR poll phases into each of
// the three teardown statuses. A concurrent `rift rm` can take a box out of a
// park transient, out of pre-ready, or out of `stopped` while the compose waits
// to start it; copying waitRunning's exit set (`failed|done|destroying`, which
// omits `ending`) or omitting the check on the post-dispatch waits burns the
// phase's whole 5-minute deadline on a settle that will never come.
//
// The post-stop row also pins that the abort happens BEFORE the start leg: a
// box being torn down must never be handed a start.
func TestRestartPollPhasesAbortOnTeardown(t *testing.T) {
	const id = "ws-1"
	stopRoute := "POST /api/workspaces/" + id + "/stop"
	startRoute := "POST /api/workspaces/" + id + "/start"

	phases := []struct {
		name string
		// script builds the GET script for a given terminal status.
		script    func(term string) []string
		wantPosts []string
	}{
		{
			name:      "pre-dispatch poll out of a park transient",
			script:    func(term string) []string { return []string{"stopping", "stopping", term} },
			wantPosts: nil,
		},
		{
			name:      "pre-dispatch poll out of pre-ready",
			script:    func(term string) []string { return []string{"starting", "starting", term} },
			wantPosts: nil,
		},
		{
			name:      "post-stop poll to stopped",
			script:    func(term string) []string { return []string{"running", "stopping", term} },
			wantPosts: []string{stopRoute},
		},
		{
			name:      "post-start poll to running",
			script:    func(term string) []string { return []string{"stopped", "resuming", term} },
			wantPosts: []string{startRoute},
		},
	}

	for _, ph := range phases {
		for _, term := range []string{"ending", "destroying", "done"} {
			t.Run(ph.name+"/"+term, func(t *testing.T) {
				cp := newRestartCP(t, id, ph.script(term))
				_, err := runRestart(t, cp, id)
				if err == nil {
					t.Fatalf("%s → %s: want an abort, got success (recorded %+v)", ph.name, term, cp.log())
				}
				for _, sub := range []string{term, "torn down"} {
					if !strings.Contains(err.Error(), sub) {
						t.Fatalf("%s → %s: error = %q, want it to contain %q", ph.name, term, err, sub)
					}
				}
				if got := cp.posts(); !routesEqual(got, ph.wantPosts) {
					t.Fatalf("%s → %s: dispatch log = %v, want %v (full log %+v)",
						ph.name, term, got, ph.wantPosts, cp.log())
				}
			})
		}
	}
}

// TestRestartStopPhaseTargetsStoppedExactly pins the stop phase's target as
// `stopped` EXACTLY, never "a rest state". `suspended` is a rest state, and
// starting from it would resume the very snapshot the restart is discarding —
// a defect no status assertion at the end of the run would catch, because the
// box does reach `running` either way.
//
// The script hands the phase a `suspended` read before `stopped`. The proof is
// positional: the GET immediately preceding the start POST must be the one that
// served `stopped`. Under a phase that accepted any rest state, the start would
// follow the `suspended` read instead.
func TestRestartStopPhaseTargetsStoppedExactly(t *testing.T) {
	const id = "ws-1"
	startRoute := "POST /api/workspaces/" + id + "/start"
	stopRoute := "POST /api/workspaces/" + id + "/stop"

	cp := newRestartCP(t, id, []string{"running", "suspended", "stopped", "running"})
	_, err := runRestart(t, cp, id)
	if err != nil {
		t.Fatalf("restart: %v (recorded %+v)", err, cp.log())
	}
	if got, want := cp.posts(), []string{stopRoute, startRoute}; !routesEqual(got, want) {
		t.Fatalf("dispatch log = %v, want %v", got, want)
	}

	log := cp.log()
	start := -1
	for i, r := range log {
		if r.Route == startRoute {
			start = i
			break
		}
	}
	if start < 1 {
		t.Fatalf("no start POST in %+v", log)
	}
	if log[start-1].Served != "stopped" {
		t.Fatalf("the start was dispatched right after a GET that served %q — the stop phase must wait for `stopped` exactly, never a rest state (log %+v)",
			log[start-1].Served, log)
	}
	sawSuspended := false
	for _, r := range log[:start] {
		if r.Served == "suspended" {
			sawSuspended = true
		}
	}
	if !sawSuspended {
		t.Fatalf("fixture is vacuous: the stop phase never observed a `suspended` read (log %+v)", log)
	}
}

// TestRestartPollIgnoresNoChangeHold covers the long-poll's 304 (hold expired,
// nothing changed) landing MID-PHASE. It is the one wire answer a poll phase
// gets that is neither a status nor an error: client.Get translates it into an
// empty workspace plus the same cursor, and the phase must re-poll and complete.
// Should that translation ever be narrowed, every poll phase in the compose
// turns a routine expired hold into a hard failure — which is what this pins.
//
// (restartPoll's `next.WorkspaceID != ""` guard is the other half of the same
// translation, and it IS observable — an earlier version of this comment said it
// was not. Remove it and the status is zeroed for one iteration; "" is in
// neither the want set nor the terminal set, so the phase keeps polling and runs
// out its deadline, which fails all three rows of
// TestRestartPhaseDeadlineGiveUpCopy on the stuck status they assert.)
func TestRestartPollIgnoresNoChangeHold(t *testing.T) {
	const id = "ws-1"
	stopRoute := "POST /api/workspaces/" + id + "/stop"
	startRoute := "POST /api/workspaces/" + id + "/start"

	cp := newRestartCP(t, id, []string{"running", "stopping", cpNotModified, "stopped", "running"})
	_, err := runRestart(t, cp, id)
	if err != nil {
		t.Fatalf("restart across a 304 hold: %v (recorded %+v)", err, cp.log())
	}
	if got, want := cp.posts(), []string{stopRoute, startRoute}; !routesEqual(got, want) {
		t.Fatalf("dispatch log = %v, want %v (full log %+v)", got, want, cp.log())
	}

	// THE CURSOR SEQUENCE, exactly — this run is the only one whose phases are
	// short enough to write out, and it is the only assertion in the file that
	// distinguishes a real long poll from a hot spin. `url.URL.Path` drops the
	// query, so an implementation that passed an EMPTY cursor on every iteration
	// — turning each 40 s hold into an immediate snapshot read, i.e. a spin
	// against the API — satisfies every route and count assertion here.
	//
	//	1 restartStatus  snapshot          → running (issues c1)
	//	  POST stop
	//	2 restartPoll#1  snapshot          → stopping (issues c2)
	//	3 restartPoll#1  long poll c2      → 304: hold expired, NO new cursor
	//	4 restartPoll#1  long poll c2 AGAIN → stopped   ← the 304's whole point
	//	  POST start
	//	5 restartPoll#2  snapshot          → running
	if got, want := cp.cursors(), []string{"", "", "c2", "c2", ""}; !routesEqual(got, want) {
		t.Fatalf("cursor sequence = %v, want %v (full log %+v)", got, want, cp.log())
	}
}

// TestRestartPhaseDeadlineGiveUpCopy pins the OTHER give-up — the one a phase
// reaches when the box simply never moves. It is the likelier give-up in
// production (a wedged provider op outlives a 409 race by minutes), and until
// the deadline was made reachable it was pinned by nothing at all: with
// restartPhaseDeadline a `const`, no test could wait five minutes, so deleting
// the whole branch left the suite green.
//
// What each row pins is the RETRY COMMAND, which differs per phase and is the
// one thing §4.3 states as a prohibition rather than a preference: a give-up in
// the STOP phase must print `rift stop --cold <id>` and must NEVER print
// `rift start <id>` — the un-park guard admits only rest states, so a start
// issued into a park transient 409s and sends the user in a circle. Swapping
// that one string is a mutation the 409-path test cannot see.
func TestRestartPhaseDeadlineGiveUpCopy(t *testing.T) {
	const id = "ws-1"

	for _, tc := range []struct {
		name       string
		script     []string // the last entry is where the box sticks
		stuckAt    string   // …and the status the give-up must name
		wantCmd    string
		wantNotCmd string
		wantPosts  []string
	}{
		{
			// Phase 1: nothing dispatched yet, so the give-up is the whole
			// command — re-running it re-enters the table from wherever the box
			// then is.
			name:      "phase 1 — a park transient that never settles",
			script:    []string{"stopping"},
			stuckAt:   "stopping",
			wantCmd:   "rift restart " + id,
			wantPosts: nil,
		},
		{
			// The stop leg. `rift start` here is the forbidden string.
			name:       "stop leg — the cold stop never lands",
			script:     []string{"running", "stopping"},
			stuckAt:    "stopping",
			wantCmd:    "rift stop --cold " + id,
			wantNotCmd: "rift start " + id,
			wantPosts:  []string{"POST /api/workspaces/" + id + "/stop"},
		},
		{
			// The start leg: the box took the start and hung coming up.
			name:      "start leg — the box never reaches running",
			script:    []string{"stopped", "resuming"},
			stuckAt:   "resuming",
			wantCmd:   "rift start " + id,
			wantPosts: []string{"POST /api/workspaces/" + id + "/start"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortRestartDeadlines(t)
			cp := newStuckCP(t, id, tc.script)

			// Bounded well above the 150 ms phase deadline, so it never fires on
			// a working compose — it exists so that DELETING the deadline branch
			// fails here in seconds instead of hanging until the test binary
			// panics. See runRestartCtx.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := runRestartCtx(t, cp, id, ctx)
			if err == nil {
				t.Fatalf("%s: want a deadline give-up, got success (log %+v)", tc.name, cp.log())
			}
			msg := err.Error()
			if !strings.Contains(msg, "did not reach") {
				t.Fatalf("%s: error = %q, want the phase-deadline give-up", tc.name, msg)
			}
			// It must name the box's ACTUAL status, not the one the compose
			// started from — that is the whole point of re-reading.
			if !strings.Contains(msg, tc.stuckAt) {
				t.Fatalf("%s: error = %q, want it to name the stuck status %q", tc.name, msg, tc.stuckAt)
			}
			if !strings.Contains(msg, tc.wantCmd) {
				t.Fatalf("%s: error = %q, want the retry command %q", tc.name, msg, tc.wantCmd)
			}
			if tc.wantNotCmd != "" && strings.Contains(msg, tc.wantNotCmd) {
				t.Fatalf("%s: error = %q, must NOT print %q — it 409s by the same guard", tc.name, msg, tc.wantNotCmd)
			}
			if got := cp.posts(); !routesEqual(got, tc.wantPosts) {
				t.Fatalf("%s: dispatch log = %v, want %v", tc.name, got, tc.wantPosts)
			}
		})
	}
}

// --- 6N4: retry-once on a dispatched 409, and the give-up copy --------------

// TestRestartRetriesIneligibleStatusOnce pins the retry budget and, more
// importantly, the WORDING of each leg's give-up. Both are live code, not
// defensive: an automatic park can flip `running → stopping` between the status
// read and the cold stop, and §4.6 row 6's corrective flips a rest-state row to
// `stopping` while mirroring NOTHING — so `rift ls` and the compose's own read
// still show the rest state while the module answers `ineligible-status`.
//
// The give-up lines are what a wrong branch would get wrong silently: the stop
// phase must print `rift stop --cold <id>` and never `rift start <id>`, which
// 409s by the same guard and sends the user in a circle. And neither may fall
// back to the raw `HTTP 409: {…}` body.
func TestRestartRetriesIneligibleStatusOnce(t *testing.T) {
	const id = "ws-1"
	stopRoute := "POST /api/workspaces/" + id + "/stop"
	startRoute := "POST /api/workspaces/" + id + "/start"

	t.Run("stop leg: one 409 then success", func(t *testing.T) {
		// GET running → stop 409 → re-read running → stop 200 → poll stopped →
		// start → poll running.
		cp := newRestartCP(t, id, []string{"running", "running", "stopped", "running"})
		cp.conflicts["stop"] = 1
		_, err := runRestart(t, cp, id)
		if err != nil {
			t.Fatalf("restart: %v (recorded %+v)", err, cp.log())
		}
		if got, want := cp.posts(), []string{stopRoute, stopRoute, startRoute}; !routesEqual(got, want) {
			t.Fatalf("dispatch log = %v, want %v", got, want)
		}
	})

	t.Run("BOTH legs 409 once: the budgets are per-leg, not shared", func(t *testing.T) {
		// The one row that distinguishes two independent counters from a single
		// shared one. §4.3 gives each leg its own retry because each has its own
		// racing producer — an automatic park can flip `running → stopping` under
		// the stop, and the un-park corrective can flip a rest-state row under the
		// start. Collapse `stopLeft`/`startLeft` into one counter and this run
		// gives up on the start leg's FIRST 409 (the retry was already spent on
		// the stop leg) — which is precisely the corrective the start-leg retry
		// exists for. Every other row in this deftest passes under that collapse.
		//
		// GET running → stop 409 → re-read running → stop 200 → poll stopped →
		// start 409 → re-read stopped → start 200 → poll running.
		cp := newRestartCP(t, id, []string{"running", "running", "stopped", "stopped", "running"})
		cp.conflicts["stop"] = 1
		cp.conflicts["start"] = 1
		_, err := runRestart(t, cp, id)
		if err != nil {
			t.Fatalf("restart with one 409 on EACH leg must still succeed — the budgets are per-leg: %v (recorded %+v)", err, cp.log())
		}
		if got, want := cp.posts(), []string{stopRoute, stopRoute, startRoute, startRoute}; !routesEqual(got, want) {
			t.Fatalf("dispatch log = %v, want %v (each leg retried exactly once)", got, want)
		}
	})

	t.Run("start leg: one 409 then success", func(t *testing.T) {
		// GET stopped → start 409 (the corrective) → re-read stopped → start 200
		// → poll running.
		cp := newRestartCP(t, id, []string{"stopped", "stopped", "running"})
		cp.conflicts["start"] = 1
		_, err := runRestart(t, cp, id)
		if err != nil {
			t.Fatalf("restart: %v (recorded %+v)", err, cp.log())
		}
		if got, want := cp.posts(), []string{startRoute, startRoute}; !routesEqual(got, want) {
			t.Fatalf("dispatch log = %v, want %v", got, want)
		}
	})

	t.Run("stop leg give-up names `rift stop --cold`, never `rift start`", func(t *testing.T) {
		// Two 409s, then restartConflict re-reads the box for the message.
		cp := newRestartCP(t, id, []string{"running", "running", "stopping"})
		cp.conflicts["stop"] = 2
		_, err := runRestart(t, cp, id)
		if err == nil {
			t.Fatalf("two refused stops must give up, got success (recorded %+v)", cp.log())
		}
		msg := err.Error()
		if !strings.Contains(msg, "rift stop --cold "+id) {
			t.Fatalf("stop give-up = %q, want it to name `rift stop --cold %s`", msg, id)
		}
		if strings.Contains(msg, "rift start") {
			t.Fatalf("stop give-up = %q, must NOT send the user to `rift start` — it 409s by the same guard", msg)
		}
		// The box's ACTUAL status, read fresh — not the raw transport error.
		if !strings.Contains(msg, "stopping") {
			t.Fatalf("stop give-up = %q, want it to report the box's actual status", msg)
		}
		if strings.Contains(msg, "HTTP 409") {
			t.Fatalf("stop give-up = %q, must not surface the raw 409 body", msg)
		}
		if got, want := cp.posts(), []string{stopRoute, stopRoute}; !routesEqual(got, want) {
			t.Fatalf("dispatch log = %v, want exactly two stops and no start", got)
		}
	})

	t.Run("start leg give-up names `rift start` and the status ls shows", func(t *testing.T) {
		cp := newRestartCP(t, id, []string{"stopped", "stopped", "stopped"})
		cp.conflicts["start"] = 2
		_, err := runRestart(t, cp, id)
		if err == nil {
			t.Fatalf("two refused starts must give up, got success (recorded %+v)", cp.log())
		}
		msg := err.Error()
		if !strings.Contains(msg, "rift start "+id) {
			t.Fatalf("start give-up = %q, want it to name `rift start %s`", msg, id)
		}
		if !strings.Contains(msg, "stopped") {
			t.Fatalf("start give-up = %q, want it to report the status the list shows", msg)
		}
		if strings.Contains(msg, "HTTP 409") {
			t.Fatalf("start give-up = %q, must not surface the raw 409 body", msg)
		}
		if got, want := cp.posts(), []string{startRoute, startRoute}; !routesEqual(got, want) {
			t.Fatalf("dispatch log = %v, want exactly two starts", got)
		}
	})

	// The retry is keyed on the body's `error` code, NOT on the 409 status: the
	// sibling lifecycle rejections are answers about the world (no capacity, a
	// superseding op), not races, so retrying them would double-dispatch and
	// then lie about why it failed.
	t.Run("a sibling 409 on the start leg is surfaced unretried", func(t *testing.T) {
		cp := newRestartCP(t, id, []string{"stopped"})
		cp.conflictBody = `{"error":"no-healthy-relay","detail":"no ready relay in the region"}`
		cp.conflicts["start"] = 1
		_, err := runRestart(t, cp, id)
		if err == nil {
			t.Fatalf("a no-healthy-relay 409 must surface, got success (recorded %+v)", cp.log())
		}
		if !strings.Contains(err.Error(), "no-healthy-relay") {
			t.Fatalf("error = %q, want the server's own answer", err)
		}
		if got, want := cp.posts(), []string{startRoute}; !routesEqual(got, want) {
			t.Fatalf("dispatch log = %v, want exactly one start (no retry)", got)
		}
	})

	t.Run("a sibling 409 on the stop leg is surfaced unretried", func(t *testing.T) {
		cp := newRestartCP(t, id, []string{"running"})
		cp.conflictBody = `{"error":"not-idle","detail":"workspace is not idle"}`
		cp.conflicts["stop"] = 1
		_, err := runRestart(t, cp, id)
		if err == nil {
			t.Fatalf("a not-idle 409 must surface, got success (recorded %+v)", cp.log())
		}
		if !strings.Contains(err.Error(), "not-idle") {
			t.Fatalf("error = %q, want the server's own answer", err)
		}
		if got, want := cp.posts(), []string{stopRoute}; !routesEqual(got, want) {
			t.Fatalf("dispatch log = %v, want exactly one stop and no start", got)
		}
	})
}

// --- 6N5: restart is refused in-VM -----------------------------------------

// TestCmdRestartRefusedInVM pins the machine-token gate. restart's second half
// IS `start`, which the machine bearer cannot issue — so the refusal has to be
// legible and UP FRONT, not halfway through a compose that has already
// cold-parked the box. The proof is that no route is hit at all.
func TestCmdRestartRefusedInVM(t *testing.T) {
	const wsID = "ws-self"
	for _, args := range [][]string{nil, {wsID}} {
		name := "no id"
		if len(args) > 0 {
			name = "own id"
		}
		t.Run(name, func(t *testing.T) {
			var seen []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = append(seen, r.Method+" "+r.URL.Path)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			}))
			defer srv.Close()

			machineEnv(t, wsID)
			t.Setenv("RIFT_API_URL", srv.URL)
			t.Setenv("RIFT_TOKEN", "machine-tok")

			err := cmdRestart(context.Background(), args)
			if err == nil {
				t.Fatal("in-VM restart: expected a refusal, got success")
			}
			if !strings.Contains(err.Error(), "not available in-VM") {
				t.Fatalf("in-VM restart: error = %q, want the standard in-VM refusal", err)
			}
			if !strings.Contains(err.Error(), "restart") {
				t.Fatalf("in-VM restart: error = %q, want it to name the verb", err)
			}
			if len(seen) != 0 {
				t.Fatalf("in-VM restart must hit NO route — it must not cold-park the box first; recorded %v", seen)
			}
		})
	}
}

// TestRestartConflictCopyEdgeCases pins the two restartConflict branches whose
// comments assert they are live but which no other row reaches. Both survived a
// deletion mutation before this existed.
//
// The label matters as much as the branch: the status comes from
// GET /api/workspaces/{id} — the row — while `rift ls` renders the mirror, and
// row 6's corrective flips one without the other. Copy that says "rift ls shows"
// over a row read is wrong in exactly the race the copy exists for.
func TestRestartConflictCopyEdgeCases(t *testing.T) {
	const id = "ws-1"

	t.Run("a re-read that fails carries the error, not a settling claim", func(t *testing.T) {
		gets := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, cpIneligible)
				return
			}
			gets++
			// GET 1 is the compose's opening status read and GET 2 is the
			// re-read after the FIRST 409 — both must succeed, or the retry loop
			// returns their error and restartConflict is never reached. The
			// SECOND 409 exhausts the leg's budget; GET 3 is restartConflict's
			// own re-read, and that is the one that fails here.
			if gets <= 2 {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"workspace": map[string]any{"workspace_id": id, "status": "running"}, "cursor": "c1"})
				return
			}
			w.WriteHeader(http.StatusNotFound) // a concurrent `rift rm`
			_, _ = io.WriteString(w, `{"error":"not-found"}`)
		}))
		defer srv.Close()

		err := restart(context.Background(), client.New(srv.URL, "tok"), id)
		if err == nil {
			t.Fatalf("want an error when the re-read fails")
		}
		if strings.Contains(err.Error(), "still settling") {
			t.Fatalf("a FAILED re-read must not claim the box is still settling: %q", err)
		}
		if !strings.Contains(err.Error(), "re-reading its status failed") {
			t.Fatalf("error = %q, want it to name the failed re-read", err)
		}
	})

	t.Run("a 200 with no workspace object does not render an empty status", func(t *testing.T) {
		gets := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, cpIneligible)
				return
			}
			gets++
			// Same shape as above: GETs 1 and 2 carry the compose to its second
			// 409, and GET 3 — restartConflict's re-read — is the degenerate one.
			if gets <= 2 {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"workspace": map[string]any{"workspace_id": id, "status": "running"}, "cursor": "c1"})
				return
			}
			_, _ = io.WriteString(w, `{}`) // degenerate: 200, no workspace object
		}))
		defer srv.Close()

		err := restart(context.Background(), client.New(srv.URL, "tok"), id)
		if err == nil {
			t.Fatalf("want the give-up error")
		}
		if strings.Contains(err.Error(), "reports: )") || strings.Contains(err.Error(), "reports: .") {
			t.Fatalf("a body with no workspace must not render an empty status: %q", err)
		}
		if !strings.Contains(err.Error(), "still settling") {
			t.Fatalf("error = %q, want the no-status settling line", err)
		}
	})
}
