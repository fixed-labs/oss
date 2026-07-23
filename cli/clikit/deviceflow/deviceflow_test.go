package deviceflow

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fixed-labs/oss/cli/clikit/httpx"
)

// testClient spins a throwaway httptest server against h and returns an
// httpx.Client pointed at it (mirrors internal/client's testClient helper).
func testClient(t *testing.T, h http.HandlerFunc) *httpx.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return httpx.New(srv.URL, "dev-bearer")
}

// a 200 poll body of {token} ONLY (no context key) yields the token. The
// device-poll no longer bakes a context into the grant (context is resolved
// per-command); this pins that a bare-token body decodes cleanly.
func TestDevicePollTokenOnlyBody(t *testing.T) {
	hc := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "minted"})
	})
	tok, err := Poll(context.Background(), hc, "DC")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if tok.Token != "minted" {
		t.Fatalf("token: %+v", tok)
	}
}

func TestDeviceStartDefaultsInterval(t *testing.T) {
	hc := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "DC", "user_code": "ABCD-1234",
			"verification_url": "https://api/activate", "interval": 0,
		})
	})
	start, err := Start(context.Background(), hc)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if start.IntervalSeconds != 5 { // defaulted from 0
		t.Fatalf("interval default: %d", start.IntervalSeconds)
	}
}

// 204 (the server's hold-timeout signal) is the pending case; 202 is still
// accepted for resilience. Both must surface ErrAuthPending; a following 200
// yields the token.
func TestDevicePollPendingThenToken(t *testing.T) {
	// poll 0 → 204, poll 1 → 202, poll 2 → 200.
	n := 0
	hc := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch n {
		case 0:
			n++
			w.WriteHeader(http.StatusNoContent) // 204 — hold-timeout, re-poll
		case 1:
			n++
			w.WriteHeader(http.StatusAccepted) // 202 — accepted for resilience
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "minted"})
		}
	})
	if _, err := Poll(context.Background(), hc, "DC"); !errors.Is(err, ErrAuthPending) {
		t.Fatalf("poll on 204: want ErrAuthPending, got %v", err)
	}
	if _, err := Poll(context.Background(), hc, "DC"); !errors.Is(err, ErrAuthPending) {
		t.Fatalf("poll on 202: want ErrAuthPending, got %v", err)
	}
	tok, err := Poll(context.Background(), hc, "DC")
	if err != nil {
		t.Fatalf("poll on 200: %v", err)
	}
	if tok.Token != "minted" {
		t.Fatalf("token: %+v", tok)
	}
}

// A single poll tolerates the server holding the connection briefly (a long-
// poll) then answering 200 — the ~35s per-poll timeout is well above any short
// hold. The hold here is tiny so the test stays fast/deterministic.
func TestDevicePollToleratesHeldConnection(t *testing.T) {
	hc := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond) // simulate a brief server-side hold
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "minted"})
	})
	tok, err := Poll(context.Background(), hc, "DC")
	if err != nil {
		t.Fatalf("Poll over held connection: %v", err)
	}
	if tok.Token != "minted" {
		t.Fatalf("token: %+v", tok)
	}
}

// PollUntilToken re-polls on 204 without a long fixed sleep. The old code slept
// a 5s ticker interval between polls; the long-poll reshape must not. The
// server answers 204 instantly here, so the ≤1s defensive floor applies once —
// but never the old 5s.
func TestPollUntilTokenRepollsImmediatelyOn204(t *testing.T) {
	calls := 0
	hc := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusNoContent) // hold-timeout; re-poll
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "minted"})
	})
	began := time.Now()
	tok, err := PollUntilToken(context.Background(), hc, &DeviceStart{DeviceCode: "DC", IntervalSeconds: 5})
	if err != nil {
		t.Fatalf("PollUntilToken: %v", err)
	}
	if tok.Token != "minted" {
		t.Fatalf("token: %+v", tok)
	}
	if calls != 2 {
		t.Fatalf("expected 2 polls (204 then 200), got %d", calls)
	}
	// The instant-204 path trips the ≤1s defensive floor exactly once; assert
	// we're nowhere near the old 5s ticker.
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Fatalf("re-poll took too long (%v) — fixed ticker not dropped", elapsed)
	}
}

func TestPollUntilTokenReturnsImmediatelyOnApproval(t *testing.T) {
	hc := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "minted"})
	})
	began := time.Now()
	tok, err := PollUntilToken(context.Background(), hc, &DeviceStart{DeviceCode: "DC", IntervalSeconds: 5})
	if err != nil {
		t.Fatalf("PollUntilToken: %v", err)
	}
	if tok.Token != "minted" {
		t.Fatalf("token: %+v", tok)
	}
	// Immediate 200: no wait at all (the defensive floor only applies after a
	// pending poll, not before the first one).
	if elapsed := time.Since(began); elapsed > time.Second {
		t.Fatalf("approval should return immediately, took %v", elapsed)
	}
}

func TestDevicePollExpiredIsError(t *testing.T) {
	hc := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "expired", http.StatusGone)
	})
	_, err := Poll(context.Background(), hc, "DC")
	var ae *httpx.APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusGone {
		t.Fatalf("want 410 APIError, got %v", err)
	}
}

// The outer login deadline cancelling is terminal — PollUntilToken returns the
// ctx error, not ErrAuthPending, even though the server keeps answering 204.
func TestPollUntilTokenStopsOnOuterDeadline(t *testing.T) {
	hc := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent) // always pending
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := PollUntilToken(ctx, hc, &DeviceStart{DeviceCode: "DC", IntervalSeconds: 5})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
}

// --- Poll transport-error classification ---
//
// When c.HTTP.Do returns a transport error, Poll must distinguish:
//   - the per-request deadline fired but the OUTER login ctx is still alive
//     (reqCtx.Err() != nil && ctx.Err() == nil) ⇒ ErrAuthPending (re-poll).
//   - the outer ctx is done ⇒ propagate (terminal).
//   - a genuinely broken connection (neither deadline fired) ⇒ propagate
//     (terminal) — must NOT be misclassified as pending, else PollUntilToken
//     spins forever.
// These tests pin each sub-branch. The injectable PollTimeout lets the first
// case fire deterministically without the production 35s wait.

// reqCtx (the per-request PollTimeout) fires mid-hold while the outer login ctx
// is still alive ⇒ re-poll (ErrAuthPending), NOT a terminal error. This
// directly exercises `reqCtx.Err() != nil && ctx.Err() == nil`.
func TestDevicePollPerRequestTimeoutWhileOuterAliveIsPending(t *testing.T) {
	hc := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Sleep well past PollTimeout so the per-request deadline fires first.
		time.Sleep(300 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "minted"})
	})
	hc.PollTimeout = 50 * time.Millisecond // per-request deadline well under the hold
	// Long-lived outer ctx so only the per-request deadline can fire.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := Poll(ctx, hc, "DC")
	if !errors.Is(err, ErrAuthPending) {
		t.Fatalf("per-request timeout w/ outer ctx alive: want ErrAuthPending, got %v", err)
	}
}

// A genuinely broken connection (connection refused) ⇒ terminal error, NOT
// ErrAuthPending. Neither deadline fired, so PollUntilToken must stop rather
// than spin. We bind+close a listener to obtain an address nothing listens on.
func TestDevicePollConnectionRefusedIsTerminal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // free the port so dials get refused, deterministically

	hc := httpx.New("http://"+addr, "dev-bearer")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = Poll(ctx, hc, "DC")
	if err == nil {
		t.Fatal("connection refused: want a terminal error, got nil")
	}
	if errors.Is(err, ErrAuthPending) {
		t.Fatalf("connection refused must NOT be classified pending (would spin): %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("outer ctx should still be alive (refusal is instant); ctx.Err()=%v", ctx.Err())
	}
}

// The outer login ctx is already done ⇒ terminal: Poll propagates the ctx
// error and must NOT return ErrAuthPending.
func TestDevicePollOuterCtxDoneIsTerminal(t *testing.T) {
	hc := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // outer ctx already done before the poll
	_, err := Poll(ctx, hc, "DC")
	if errors.Is(err, ErrAuthPending) {
		t.Fatalf("outer ctx done must be terminal, not ErrAuthPending: %v", err)
	}
	if err == nil {
		t.Fatal("outer ctx done: want a terminal error, got nil")
	}
}

// End-to-end: a connection-refused endpoint makes PollUntilToken return a
// terminal error PROMPTLY (it doesn't spin on a misclassified-pending), bounded
// by a short outer ctx. Guards the Poll classification at the loop level.
func TestPollUntilTokenStopsOnConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	hc := httpx.New("http://"+addr, "dev-bearer")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	began := time.Now()
	_, err = PollUntilToken(ctx, hc, &DeviceStart{DeviceCode: "DC", IntervalSeconds: 5})
	if err == nil {
		t.Fatal("connection refused: want a terminal error, got nil")
	}
	if errors.Is(err, ErrAuthPending) {
		t.Fatalf("PollUntilToken must surface the terminal error, not ErrAuthPending: %v", err)
	}
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Fatalf("PollUntilToken spun on connection refused (took %v)", elapsed)
	}
}
