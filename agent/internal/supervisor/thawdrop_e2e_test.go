package supervisor

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fixed-labs/oss/agent/internal/api"
)

// End-to-end: after a thaw, does the forced heartbeat actually land on a NEW
// connection?
//
// Every other test of the drop uses a mock API and can only observe that
// CloseIdleConnections was *called*, in the right order. That is not the property
// anyone cares about. The property is that the dead connection is gone, and the
// only way to see it is to count connections server-side with a real
// *http.Client and a real HTTP/2 server.
//
// Why this is the sharp edge: cancelling a request does NOT synchronously remove
// its stream from the h2 connection. net/http's roundTrip aborts the stream and
// returns, but the stream is unregistered by forgetStreamID inside
// cleanupWriteRequest, which runs on a DIFFERENT goroutine after RoundTrip has
// already returned. CloseIdleConnections reaches closeIfIdle, which bails out
// while len(cc.streams) > 0 — so a drop issued on the very next line is a no-op,
// and thaw's forced beat rides the same corpse it was supposed to escape.
func TestThawForcedBeatLandsOnAFreshConnection(t *testing.T) {
	var newConns atomic.Int64
	poll := make(chan struct{}, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/rift/v1/ws-1/agent-config", func(w http.ResponseWriter, r *http.Request) {
		select {
		case poll <- struct{}{}:
		default:
		}
		<-r.Context().Done() // hold like the real ≤25s long-poll
	})
	mux.HandleFunc("/api/rift/v1/ws-1/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			newConns.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	c := api.New(srv.URL, "ws-1", "tok")
	tr := c.HTTP.Transport.(*http.Transport)
	tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	// Long ping timeouts: this test must measure the DROP, not PR 1's ping
	// backstop quietly cleaning up after it.
	tr.HTTP2 = &http.HTTP2Config{SendPingTimeout: time.Hour, PingTimeout: time.Hour}

	s := &Supervisor{API: c, Reconcile: &recordingReconciler{}}
	s.Log = discardLogger()
	s.PollFloor = time.Millisecond
	s.BackoffMin = time.Millisecond
	s.BackoffMax = 2 * time.Millisecond
	s.defaults()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.pullLoop(ctx) }()
	defer func() { cancel(); <-done }()

	select {
	case <-poll:
	case <-time.After(5 * time.Second):
		t.Fatal("the long-poll never reached the server")
	}
	if got := newConns.Load(); got != 1 {
		t.Fatalf("setup: server saw %d connections, want 1", got)
	}

	// The whole point: cancel the in-flight poll, drop the pool, force a beat.
	s.cancelPollAndDropPool()
	if err := c.Heartbeat(context.Background(), false, 0, api.Identity{}); err != nil {
		t.Fatalf("forced beat: %v", err)
	}

	if got := newConns.Load(); got < 2 {
		t.Fatalf("after the drop the forced beat reused the SAME connection (server saw %d "+
			"connections total, want ≥2). Cancelling a request does not synchronously remove "+
			"its stream — forgetStreamID runs on another goroutine after RoundTrip returns — so "+
			"closeIfIdle still saw len(cc.streams) > 0 and the drop was a no-op. On a resume the "+
			"beat that flips the row to `running` then rides the dead connection until the ping "+
			"config kills it ~10s later", got)
	}
}
