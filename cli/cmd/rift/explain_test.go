package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/fixed-labs/oss/cli/internal/client"
	"github.com/fixed-labs/oss/cli/internal/config"
)

// ---------------------------------------------------------------------------
// The two self-explaining refusals PR 6 ships (FIX-316 / park ladder WS6c).
//
// BOTH are keyed on the body's stable `error` string, never on the HTTP status,
// and in both cases a sibling response carries the SAME status with a different
// meaning. Keying on the code would therefore not fail loudly — it would print
// confident, wrong guidance:
//
//   429 workspace-cap-reached → the quota text
//   429 claim-rejected        → NOT the quota text (its reason is by
//                               construction indistinguishable: cap, credit or
//                               repo access)
//   409 ineligible-status     → "mid-transition, retry in a few seconds"
//   409 no-healthy-relay / superseded → NOT that; they are answers about the
//                               world, not races
//
// Every case below asserts the actual TEXT (or the untouched error), because
// the wrong branches all still produce "an error".
// ---------------------------------------------------------------------------

// TestWorkspaceCapMessageKeyedOnErrorStringNotStatus pins the 429 mapping.
//
// If it were keyed on the status code:
//   - the claim-rejected case would print the quota remedy over a rejection
//     that may have nothing to do with the quota — the assertion that the
//     error is returned UNCHANGED fails;
//   - the "same code under a different status" case would fall through to the
//     status-based fallbacks and print an image message instead of the quota
//     text — that assertion fails too.
func TestWorkspaceCapMessageKeyedOnErrorStringNotStatus(t *testing.T) {
	t.Run("429 workspace-cap-reached says what to do", func(t *testing.T) {
		err := explainCreate(&client.APIError{
			Status: 429,
			Body:   `{"error":"workspace-cap-reached","limit":10}`,
		}, "org/app")
		if err == nil {
			t.Fatal("want an error")
		}
		msg := err.Error()
		// The design publishes this text verbatim (and reference/06-box-lifecycle.md
		// republishes the same fact); these are its load-bearing parts.
		for _, sub := range []string{
			"10 of 10",
			"not deleted automatically",
			"rift ls",
			"rift rm <id>",
		} {
			if !strings.Contains(msg, sub) {
				t.Fatalf("cap message = %q, want it to contain %q", msg, sub)
			}
		}
		if strings.Contains(msg, "HTTP 429") {
			t.Fatalf("cap message = %q, must not leave the raw JSON in front of the user", msg)
		}
	})

	t.Run("a 429 claim-rejected is NOT given the quota text", func(t *testing.T) {
		in := &client.APIError{
			Status: 429,
			Body:   `{"error":"claim-rejected","detail":"the claim token was rejected"}`,
		}
		out := explainCreate(in, "org/app")
		if !errors.Is(out, in) {
			t.Fatalf("a claim-rejected 429 must surface unchanged, got %v", out)
		}
		if strings.Contains(out.Error(), "not deleted automatically") || strings.Contains(out.Error(), "rift rm <id>") {
			t.Fatalf("claim-rejected got the quota guidance (%q) — its reason is indistinguishable, so that text would be a guess", out)
		}
	})

	t.Run("the mapping follows the code, not the status", func(t *testing.T) {
		// Same error string under a different status: still the quota text.
		// Keyed on 429 this would fall into the 409 fallback's image message.
		err := explainCreate(&client.APIError{
			Status: 409,
			Body:   `{"error":"workspace-cap-reached","limit":3}`,
		}, "org/app")
		if err == nil || !strings.Contains(err.Error(), "3 of 3") {
			t.Fatalf("workspace-cap-reached must map on the error string alone, got %v", err)
		}
		if strings.Contains(err.Error(), "image") {
			t.Fatalf("message = %q, want the quota text, not the status-based image fallback", err)
		}
	})

	t.Run("a body with no limit drops the numbers rather than inventing them", func(t *testing.T) {
		err := explainCreate(&client.APIError{Status: 429, Body: `{"error":"workspace-cap-reached"}`}, "org/app")
		if err == nil {
			t.Fatal("want an error")
		}
		msg := err.Error()
		if strings.Contains(msg, "0 of 0") {
			t.Fatalf("an older server without `limit` must not be rendered as %q", msg)
		}
		for _, sub := range []string{"box limit", "rift ls", "rift rm <id>"} {
			if !strings.Contains(msg, sub) {
				t.Fatalf("limit-less cap message = %q, want it to contain %q", msg, sub)
			}
		}
	})

	t.Run("an unrelated 429 is unchanged", func(t *testing.T) {
		in := &client.APIError{Status: 429, Body: `not json at all`}
		if out := explainCreate(in, "org/app"); !errors.Is(out, in) {
			t.Fatalf("an undecodable 429 must surface unchanged, got %v", out)
		}
	})
}

// TestExplainStartConflictCopy pins `rift start`'s 409 copy.
//
// `ineligible-status` is the one that matters: a start can race the un-park
// corrective, which flips a rest-state row to `stopping` and mirrors NOTHING —
// so `rift ls` still shows the box at rest while the module refuses the start.
// "The box is not stopped" would read as a lie against the list the user just
// looked at; the truth is that it is mid-transition and the same command works
// a few seconds later.
//
// Keyed on the 409 status instead of the `error` string, the no-healthy-relay
// and superseded rows below would be dressed as a race and told to "retry in a
// few seconds" — real capacity answers presented as transient. Those rows
// assert the error is returned UNCHANGED, so that swap fails them.
func TestExplainStartConflictCopy(t *testing.T) {
	const id = "ws-1"
	conflict := func(code, detail string) *client.APIError {
		b, _ := json.Marshal(map[string]string{"error": code, "detail": detail})
		return &client.APIError{Status: http.StatusConflict, Body: string(b)}
	}

	cases := []struct {
		name        string
		in          error
		unchanged   bool
		wantSubs    []string
		wantNotSubs []string
	}{
		{
			name:        "a park transient gets the mid-transition line and the retry",
			in:          conflict("ineligible-status", "workspace status: stopping"),
			wantSubs:    []string{"mid-transition", "stopping", "Retry in a few seconds", "rift start " + id},
			wantNotSubs: []string{"not stopped"},
		},
		{
			name:     "a suspending row too",
			in:       conflict("ineligible-status", "workspace status: suspending"),
			wantSubs: []string{"mid-transition", "suspending", "rift start " + id},
		},
		{
			// A settled status must NOT be described as mid-transition — the
			// guidance would be false and the retry would never succeed.
			name:        "running gets its own line",
			in:          conflict("ineligible-status", "workspace status: running"),
			wantSubs:    []string{"already running"},
			wantNotSubs: []string{"mid-transition"},
		},
		{
			name:        "ending is reported as a teardown",
			in:          conflict("ineligible-status", "workspace status: ending"),
			wantSubs:    []string{"torn down", "ending"},
			wantNotSubs: []string{"mid-transition"},
		},
		{
			name:        "destroying is reported as a teardown",
			in:          conflict("ineligible-status", "workspace status: destroying"),
			wantSubs:    []string{"torn down", "destroying"},
			wantNotSubs: []string{"mid-transition"},
		},
		{
			name:        "done is reported as a teardown",
			in:          conflict("ineligible-status", "workspace status: done"),
			wantSubs:    []string{"torn down", "done"},
			wantNotSubs: []string{"mid-transition"},
		},
		{
			// An older server that sends no detail must still get the guidance;
			// the status is a bonus, not the point.
			name:     "a missing detail keeps the guidance",
			in:       conflict("ineligible-status", ""),
			wantSubs: []string{"mid-transition", "rift start " + id},
		},
		{
			name:        "no-healthy-relay is a capacity answer, not a race",
			in:          conflict("no-healthy-relay", "no ready relay in the region"),
			unchanged:   true,
			wantNotSubs: []string{"mid-transition", "Retry in a few seconds"},
		},
		{
			name:        "superseded is a real answer, not a race",
			in:          conflict("superseded", "a newer op won"),
			unchanged:   true,
			wantNotSubs: []string{"mid-transition", "Retry in a few seconds"},
		},
		{
			name:        "a 409 with an undecodable body is unchanged",
			in:          &client.APIError{Status: http.StatusConflict, Body: `{"error":`},
			unchanged:   true,
			wantNotSubs: []string{"mid-transition"},
		},
		{
			// Out of credit: a real refusal with its own remedy. Dressing a 402
			// as a race would send the user into a retry loop that can't win.
			name:        "a non-409 is unchanged",
			in:          &client.APIError{Status: http.StatusPaymentRequired, Body: `{"error":"insufficient-credit"}`},
			unchanged:   true,
			wantNotSubs: []string{"mid-transition"},
		},
		{
			name:        "a transport error is unchanged",
			in:          errors.New("dial tcp: connection refused"),
			unchanged:   true,
			wantNotSubs: []string{"mid-transition"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := explainStart(tc.in, id)
			if out == nil {
				t.Fatal("explainStart dropped the error")
			}
			if tc.unchanged && !errors.Is(out, tc.in) {
				t.Fatalf("want the error surfaced unchanged, got %q", out)
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(out.Error(), sub) {
					t.Fatalf("message = %q, want it to contain %q", out, sub)
				}
			}
			for _, sub := range tc.wantNotSubs {
				if strings.Contains(out.Error(), sub) {
					t.Fatalf("message = %q, must NOT contain %q", out, sub)
				}
			}
		})
	}

	t.Run("a successful start stays nil", func(t *testing.T) {
		if err := explainStart(nil, id); err != nil {
			t.Fatalf("explainStart(nil) = %v, want nil", err)
		}
	})
}

// TestLifecycleStartWiresExplainStart proves the mapping is actually REACHED
// from the verb the user types: the pure table above would stay green if
// `rift start` stopped calling explainStart and printed the raw 409 body.
func TestLifecycleStartWiresExplainStart(t *testing.T) {
	const id = "ws-1"
	var seen []string
	srv := hermeticEnv(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"ineligible-status","detail":"workspace status: stopping"}`))
	})
	seedConfig(t, &config.Config{APIBaseURL: srv.URL, Token: "tok"})

	var err error
	_ = captureStdout(t, func() { err = lifecycle(context.Background(), []string{id}, "start") })
	if err == nil {
		t.Fatal("rift start against a 409: want an error")
	}
	if !strings.Contains(err.Error(), "mid-transition") {
		t.Fatalf("rift start error = %q, want the mid-transition guidance", err)
	}
	if strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("rift start error = %q, must not surface the raw 409 body", err)
	}
	if want := "POST /api/workspaces/" + id + "/start"; len(seen) != 1 || seen[0] != want {
		t.Fatalf("recorded routes = %v, want exactly [%q]", seen, want)
	}
}
