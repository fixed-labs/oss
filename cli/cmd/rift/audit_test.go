package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/fixed-labs/oss/cli/internal/client"
	"github.com/fixed-labs/oss/cli/internal/config"
)

// TestAuditorPostsOnePerSecret confirms the auditor emits one secret-access event
// per --secret and that a non-2xx response is swallowed (best-effort, non-fatal).
func TestAuditorPostsOnePerSecret(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ad := &auditor{c: client.New(srv.URL, "machine-tok"), id: "ws-1"}
	exit := 2
	ad.post(context.Background(), []string{"aws", "npm"}, "make build", &exit, false)

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("want 2 events (one per secret), got %d", len(bodies))
	}
	secretsSeen := map[string]bool{}
	for _, b := range bodies {
		if b["outcome"] != "failure" || b["command"] != "make build" {
			t.Fatalf("body: %+v", b)
		}
		secretsSeen[b["secret"].(string)] = true
	}
	if !secretsSeen["aws"] || !secretsSeen["npm"] {
		t.Fatalf("secrets seen: %v", secretsSeen)
	}
}

// TestAuditorDisabledIsNoop confirms an auditor without a machine identity does
// nothing (no panic, no post) — auditing must not require an identity.
func TestAuditorDisabledIsNoop(t *testing.T) {
	ad := newAuditor(&config.Config{}) // no MachineWorkspaceID/token/url
	if ad.c != nil {
		t.Fatal("auditor should be disabled without a machine identity")
	}
	ad.post(context.Background(), []string{"aws"}, "ls", nil, true) // must not panic
}

// TestAuditorPostNeverPanicsOnServerError confirms a server error is swallowed.
func TestAuditorPostNeverPanicsOnServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	ad := &auditor{c: client.New(srv.URL, "tok"), id: "ws-1"}
	exit := 0
	ad.post(context.Background(), []string{"aws"}, "ls", &exit, true) // must return cleanly
}
