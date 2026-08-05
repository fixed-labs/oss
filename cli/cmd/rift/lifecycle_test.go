package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRetiredVerbsHintAndHitNoRoute (T1.6) drives the REAL main() dispatch for
// the retired `rift suspend` / `rift resume` verbs. The rename-hint errors live
// in main()'s top-level switch (NOT in lifecycle()), so they are only reachable
// through main() — we re-exec this already-compiled test binary via os.Args[0]
// under the RIFT_TEST_RUN_MAIN harness in version_test.go's TestMain.
//
// Each retired verb must: (a) exit non-zero, (b) print a rename hint to stderr
// containing the pinned substring (`use: rift stop` / `use: rift start` — we
// assert the substring, not the full em-dash sentence, so a copy tweak doesn't
// break the test), and (c) hit NO HTTP route. We prove (c) by pointing the child
// at a recording server and asserting it recorded zero requests: the retired
// verbs error in the switch before any client is built.
func TestRetiredVerbsHintAndHitNoRoute(t *testing.T) {
	cases := []struct {
		name    string
		verb    string
		wantSub string
	}{
		{name: "suspend renamed to stop", verb: "suspend", wantSub: "use: rift stop"},
		{name: "resume renamed to start", verb: "resume", wantSub: "use: rift start"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A recording server — the child should never reach it for a
			// retired verb. If it does, hits > 0 fails the test.
			var hits int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			}))
			defer srv.Close()

			cmd := exec.Command(os.Args[0], tc.verb, "ws-1")
			// Machine-mode env so that, if the switch ever regressed to routing
			// the retired verb into lifecycle(), a client WOULD be built against
			// srv.URL and record a hit — making the "no route" assertion real.
			cmd.Env = []string{
				"RIFT_TEST_RUN_MAIN=1",
				"RIFT_LOG_FILE=" + filepath.Join(t.TempDir(), "rift.log"),
				"XDG_CONFIG_HOME=" + t.TempDir(),
				"HOME=" + t.TempDir(),
				"RIFT_WORKSPACE_ID=ws-1",
				"RIFT_API_URL=" + srv.URL,
				"RIFT_TOKEN=machine-tok",
			}

			var stderr bytes.Buffer
			cmd.Stderr = &stderr

			err := cmd.Run()
			if err == nil {
				t.Fatalf("rift %s: expected a non-zero exit, got success", tc.verb)
			}
			if _, ok := err.(*exec.ExitError); !ok {
				t.Fatalf("rift %s: expected an ExitError (non-zero exit), got %v", tc.verb, err)
			}
			if got := stderr.String(); !strings.Contains(got, tc.wantSub) {
				t.Fatalf("rift %s: stderr = %q, want it to contain %q", tc.verb, got, tc.wantSub)
			}
			if hits != 0 {
				t.Fatalf("rift %s: a retired verb must hit NO HTTP route, but the server recorded %d request(s)", tc.verb, hits)
			}
		})
	}
}

// TestRestartVerbIsReachableFromTheCLI pins the one thing 800-odd lines of
// restart_test.go cannot: that `restart` is WIRED INTO main()'s dispatch switch.
// Every other test in this package calls restart()/cmdRestart() as a function,
// so deleting `case "restart":` leaves the whole suite green while
// `rift restart <id>` becomes `unknown command`, exit 2 — a total outage of this
// PR's headline verb, shipping green.
//
// It asserts through the IN-VM REFUSAL rather than a happy path, because that is
// the one restart outcome reachable with no control plane at all: cmdRestart
// checks MachineWorkspaceID before it builds a client. The recording server then
// proves the same thing it proves for the retired verbs above — the verb was
// dispatched and refused, not quietly routed somewhere else.
func TestRestartVerbIsReachableFromTheCLI(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	cmd := exec.Command(os.Args[0], "restart", "ws-1")
	cmd.Env = []string{
		"RIFT_TEST_RUN_MAIN=1",
		"RIFT_LOG_FILE=" + filepath.Join(t.TempDir(), "rift.log"),
		"XDG_CONFIG_HOME=" + t.TempDir(),
		"HOME=" + t.TempDir(),
		"RIFT_WORKSPACE_ID=ws-1",
		"RIFT_API_URL=" + srv.URL,
		"RIFT_TOKEN=machine-tok",
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		t.Fatalf("rift restart in-VM: expected a non-zero exit, got success (stderr %q)", stderr.String())
	}
	if _, ok := err.(*exec.ExitError); !ok {
		t.Fatalf("rift restart: expected an ExitError, got %v", err)
	}
	got := stderr.String()
	// `unknown command` is the exact symptom of the missing switch arm, and it is
	// asserted against by name so a future copy change to the refusal cannot let
	// that failure mode pass as "well, some error occurred".
	if strings.Contains(got, "unknown command") {
		t.Fatalf("rift restart is not wired into main()'s dispatch switch: stderr = %q", got)
	}
	if !strings.Contains(got, "not available in-VM") {
		t.Fatalf("rift restart in-VM: stderr = %q, want the in-VM refusal", got)
	}
	if hits != 0 {
		t.Fatalf("rift restart in-VM must hit NO HTTP route (it refuses before building a client), but the server recorded %d request(s)", hits)
	}
}

// machineEnv puts lifecycle() in machine (in-VM) mode: XDG_CONFIG_HOME/HOME
// point at an empty temp dir so config.Load() finds no saved login, and
// RIFT_WORKSPACE_ID flags machine mode. The caller sets RIFT_API_URL +
// RIFT_TOKEN (the machine credentials) so Config.Validate passes.
func machineEnv(t *testing.T, wsID string) {
	t.Helper()
	d := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", d)
	t.Setenv("HOME", d)
	t.Setenv("RIFT_WORKSPACE_ID", wsID)
}

// TestInVMLifecycleAllowsStopOnly (T1.7) drives lifecycle() directly in machine
// mode — lifecycle() IS the real in-VM gate (`if verb != "stop"`). In-VM only
// `stop` is allowed; it must reach the machine agent route
// POST /api/rift/v1/{id}/stop. Any non-`stop` verb (here `start`) is rejected
// with an error and hits no route. We assert observable behavior — the recorded
// HTTP method+path for the allowed verb, and (error, no recorded request) for
// the rejected one — and pin only the stable `not available in-VM` substring,
// never the full sentence.
func TestInVMLifecycleAllowsStopOnly(t *testing.T) {
	const wsID = "ws-self"

	t.Run("stop is allowed and hits the machine route", func(t *testing.T) {
		var seen []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = append(seen, r.Method+" "+r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}))
		defer srv.Close()

		machineEnv(t, wsID)
		t.Setenv("RIFT_API_URL", srv.URL)
		t.Setenv("RIFT_TOKEN", "machine-tok")

		if err := lifecycle(context.Background(), nil, "stop"); err != nil {
			t.Fatalf("in-VM stop: expected success, got %v", err)
		}
		want := "POST /api/rift/v1/" + wsID + "/stop"
		if len(seen) != 1 || seen[0] != want {
			t.Fatalf("in-VM stop: recorded routes = %v, want exactly [%q]", seen, want)
		}
	})

	t.Run("start is rejected in-VM and hits no route", func(t *testing.T) {
		var seen []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = append(seen, r.Method+" "+r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}))
		defer srv.Close()

		machineEnv(t, wsID)
		t.Setenv("RIFT_API_URL", srv.URL)
		t.Setenv("RIFT_TOKEN", "machine-tok")

		err := lifecycle(context.Background(), nil, "start")
		if err == nil {
			t.Fatal("in-VM start: expected a rejection error, got success")
		}
		if !strings.Contains(err.Error(), "not available in-VM") {
			t.Fatalf("in-VM start: error = %q, want it to contain %q", err.Error(), "not available in-VM")
		}
		if len(seen) != 0 {
			t.Fatalf("in-VM start: a rejected verb must hit NO route, recorded %v", seen)
		}
	})
}
