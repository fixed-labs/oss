package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/fixed-labs/oss/cli/internal/config"
)

// ---------------------------------------------------------------------------
// `rift stop --cold` on the wire (FIX-316 / park ladder WS6c).
//
// The cold override is a BODY flag, not a route: `--cold` posts {"cold":true}
// and a warm stop posts NO BODY AT ALL — the wire shape every released rift
// sends, which is what keeps old clients parking warm-when-eligible.
//
// This is invisible from the CLI's own output: a stop whose flag parsed but
// whose body never got sent still succeeds, warm. So the assertions read the
// request the SERVER received — route, body and Content-Type — not the exit
// code. (Deliberate overlap with the Clojure edge tests, which pin that the
// server honours the body: neither half implies the other, and the seam
// between them is a silent no-op.)
// ---------------------------------------------------------------------------

type stopReq struct {
	Route       string // "METHOD /path"
	Body        string
	ContentType string
}

type stopRecorder struct {
	mu   sync.Mutex
	reqs []stopReq
}

func (rec *stopRecorder) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rec.mu.Lock()
	rec.reqs = append(rec.reqs, stopReq{
		Route:       r.Method + " " + r.URL.Path,
		Body:        string(body),
		ContentType: r.Header.Get("Content-Type"),
	})
	rec.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (rec *stopRecorder) only(t *testing.T) stopReq {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.reqs) != 1 {
		t.Fatalf("want exactly one request, got %d: %+v", len(rec.reqs), rec.reqs)
	}
	return rec.reqs[0]
}

func (rec *stopRecorder) count() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return len(rec.reqs)
}

// TestStopColdSendsBodyDeveloperSurface pins the developer edge: `--cold` puts
// {"cold":true} on the wire, a bare stop puts nothing there.
func TestStopColdSendsBodyDeveloperSurface(t *testing.T) {
	const id = "ws-1"
	cases := []struct {
		name            string
		args            []string
		wantBody        string
		wantContentType string
		wantLine        string
	}{
		{
			name:            "--cold posts the override",
			args:            []string{"--cold", id},
			wantBody:        `{"cold":true}`,
			wantContentType: "application/json",
			wantLine:        id + ": stop (cold)",
		},
		{
			// The shape every released rift sends. A warm stop that started
			// posting {"cold":false} would still park warm, but it would change
			// the wire contract older servers were built against.
			name:            "a bare stop posts NO body",
			args:            []string{id},
			wantBody:        "",
			wantContentType: "",
			wantLine:        id + ": stop",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &stopRecorder{}
			srv := hermeticEnv(t, rec.handler)
			seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

			var err error
			out := captureStdout(t, func() { err = lifecycle(context.Background(), tc.args, "stop") })
			if err != nil {
				t.Fatalf("rift stop %v: %v", tc.args, err)
			}
			got := rec.only(t)
			if want := "POST /api/workspaces/" + id + "/stop"; got.Route != want {
				t.Fatalf("route = %q, want %q", got.Route, want)
			}
			if got.Body != tc.wantBody {
				t.Fatalf("request body = %q, want %q", got.Body, tc.wantBody)
			}
			if got.ContentType != tc.wantContentType {
				t.Fatalf("Content-Type = %q, want %q", got.ContentType, tc.wantContentType)
			}
			// The tier is otherwise invisible to the user, so the success line
			// is the only signal that --cold was honoured client-side.
			if !strings.Contains(out, tc.wantLine) {
				t.Fatalf("stdout = %q, want it to contain %q", out, tc.wantLine)
			}
		})
	}
}

// TestStopColdSendsBodyMachineSurface pins the in-VM twin. Both surfaces carry
// the flag so an in-box stop and a laptop stop decide the same tier; the two
// edges are separate code paths and the agent half is the least-reviewed one.
func TestStopColdSendsBodyMachineSurface(t *testing.T) {
	const wsID = "ws-self"
	cases := []struct {
		name     string
		args     []string
		wantBody string
	}{
		{name: "--cold with no id posts the override", args: []string{"--cold"}, wantBody: `{"cold":true}`},
		{name: "--cold with the own id posts the override", args: []string{"--cold", wsID}, wantBody: `{"cold":true}`},
		{name: "a bare in-VM stop posts NO body", args: nil, wantBody: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &stopRecorder{}
			srv := httptest.NewServer(http.HandlerFunc(rec.handler))
			defer srv.Close()

			machineEnv(t, wsID)
			t.Setenv("RIFT_API_URL", srv.URL)
			t.Setenv("RIFT_TOKEN", "machine-tok")

			var err error
			_ = captureStdout(t, func() { err = lifecycle(context.Background(), tc.args, "stop") })
			if err != nil {
				t.Fatalf("in-VM rift stop %v: %v", tc.args, err)
			}
			got := rec.only(t)
			// The machine bearer only opens the agent prefix — a cold stop must
			// not silently fall back to the developer route.
			if want := "POST /api/rift/v1/" + wsID + "/stop"; got.Route != want {
				t.Fatalf("route = %q, want %q", got.Route, want)
			}
			if got.Body != tc.wantBody {
				t.Fatalf("request body = %q, want %q", got.Body, tc.wantBody)
			}
		})
	}
}

// TestInVMStopRefusesAnotherWorkspacesID pins the guard that keeps an in-VM stop
// on the box's OWN workspace. The rows above pass either the own id or none, so
// every one of them is satisfied by a wiring that ignores the positional
// entirely — and that wiring is not hypothetical: lifecycle() reconstructs the
// arg slice machineTarget consumes (kong exposes only the parsed struct), so
// making that slice unconditionally nil silently retargets the local box and no
// other test notices.
//
// The consequence is a stop issued against a workspace the caller named and did
// not get: the box it is running inside parks instead, and the CLI reports
// success.
func TestInVMStopRefusesAnotherWorkspacesID(t *testing.T) {
	const wsID = "ws-self"
	rec := &stopRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	machineEnv(t, wsID)
	t.Setenv("RIFT_API_URL", srv.URL)
	t.Setenv("RIFT_TOKEN", "machine-tok")

	var err error
	_ = captureStdout(t, func() {
		err = lifecycle(context.Background(), []string{"--cold", "ws-someone-else"}, "stop")
	})
	if err == nil {
		t.Fatalf("in-VM stop of ANOTHER workspace must be refused, got success")
	}
	if !strings.Contains(err.Error(), "may only act on this workspace") {
		t.Fatalf("error = %q, want the in-VM target refusal", err)
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("the refusal must happen before any request; recorded %d", n)
	}
}

// TestColdFlagIsRejectedOnStartAndRm pins the reason `start`/`rm` parse against
// a struct with NO Cold field: `rift start --cold` must ERROR, not accept a
// flag it silently ignores. A user who typed it would otherwise believe they
// had asked for something.
func TestColdFlagIsRejectedOnStartAndRm(t *testing.T) {
	for _, verb := range []string{"start", "rm"} {
		t.Run(verb, func(t *testing.T) {
			rec := &stopRecorder{}
			srv := hermeticEnv(t, rec.handler)
			seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

			var err error
			_ = captureStdout(t, func() { err = lifecycle(context.Background(), []string{"--cold", "ws-1"}, verb) })
			if err == nil {
				t.Fatalf("rift %s --cold: want a parse error, got success", verb)
			}
			if !strings.Contains(err.Error(), "cold") {
				t.Fatalf("rift %s --cold: error = %q, want it to name the unknown flag", verb, err)
			}
			if n := rec.count(); n != 0 {
				t.Fatalf("rift %s --cold: a rejected flag must hit NO route, recorded %d", verb, n)
			}
		})
	}
}
