package broker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// requestTimeout bounds one handler exchange (read request → mint → write
// response). Source reads (esp. a cmd: source) are themselves bounded upstream
// (secrets.readSource, 30s), so this is a generous outer cushion.
const requestTimeout = 60 * time.Second

// Handler is the stateless laptop-side credential handler. It serves broker
// requests on a listener (the overlay listener from Tunnel.ListenOverlayTCP),
// resolving each
// to the static provider. It holds no state beyond the provider; everything else
// lives on the control plane.
//
// NOTE: the Handler NEVER logs a request or response body — a credential value
// must never reach a log line, a transcript, or anything durable. logf is used
// only for connection-level diagnostics (accept/serve errors), never the payload.
type Handler struct {
	prov Provider
	logf func(string, ...any) // optional; value-free diagnostics only. nil = silent.
}

// NewHandler builds a handler over a provider.
func NewHandler(prov Provider, logf func(string, ...any)) *Handler {
	return &Handler{prov: prov, logf: logf}
}

func (h *Handler) log(format string, args ...any) {
	if h.logf != nil {
		h.logf(format, args...)
	}
}

// Serve accepts connections on ln until ctx is done or ln is closed, handling
// each in its own goroutine. It returns when the listener stops accepting
// (typically because the caller closed it on session end). It is the caller's
// job to close ln; Serve does not close it.
func (h *Handler) Serve(ctx context.Context, ln net.Listener) error {
	// Close the listener when ctx is cancelled so Accept unblocks and Serve
	// returns. (The connect wiring also closes it via defer; double-close is
	// harmless on a gonet listener.)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // shutting down — not an error
			}
			return err
		}
		go h.serveConn(ctx, conn)
	}
}

// serveConn handles exactly one request/response on conn. One credential request
// is one short-lived connection.
func (h *Handler) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	cctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	_ = conn.SetDeadline(time.Now().Add(requestTimeout))

	var req Request
	if err := readFrame(conn, &req); err != nil {
		// A malformed/garbage connection: log the connection-level fault (no body)
		// and drop it. Don't bother replying — we couldn't parse the frame.
		h.log("broker: read request failed: %v", err)
		return
	}
	resp := h.Handle(cctx, req)
	if err := writeFrame(conn, resp); err != nil {
		h.log("broker: write response failed: %v", err)
	}
}

// Handle resolves one request to a Response. Exported so the loopback tests can
// exercise it directly, and so serveConn stays a thin transport wrapper. It NEVER
// returns a credential value inside Error/Detail.
func (h *Handler) Handle(ctx context.Context, req Request) Response {
	if req.Secret == "" {
		return Response{Error: ErrBadRequest, Detail: "empty secret name"}
	}
	switch req.Mode {
	case ModeInject, ModeMaterialize:
	default:
		return Response{Error: ErrBadRequest, Detail: fmt.Sprintf("unknown mode %q", req.Mode)}
	}

	cred, err := h.prov.Mint(ctx, MintRequest{Secret: req.Secret})
	if err != nil {
		var re *resolveError
		if errors.As(err, &re) {
			// Unknown / non-inject / unmapped — the message names only the secret,
			// so it is value-free and safe to surface.
			return Response{Error: ErrUnknownSecret, Detail: re.Error()}
		}
		// A read/extract failure could in principle echo source bytes in an error,
		// so DO NOT forward err's text — return a generic, value-free detail.
		h.log("broker: mint %q failed (internal)", req.Secret)
		return Response{Error: ErrInternal, Detail: "failed to read or extract the credential source"}
	}

	switch req.Mode {
	case ModeInject:
		env := make([]EnvPair, len(cred.Env))
		for i, p := range cred.Env {
			env[i] = EnvPair{Name: p.Name, Value: p.Value}
		}
		return Response{Env: env}
	case ModeMaterialize:
		return Response{Material: cred.Raw}
	default:
		return Response{Error: ErrBadRequest, Detail: "unreachable"}
	}
}
