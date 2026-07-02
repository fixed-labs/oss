package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// Device-flow login (the OAuth-device-grant shape, against the server's
// login endpoints):
//
//	POST /api/login/device/start              → {device_code, user_code, verification_url, interval}
//	POST /api/login/device/poll {device_code} → 200 {token} | 204 (pending, hold-timeout) | 4xx (expired/denied)
//
// The poll is a server-side LONG-poll: the api holds each request up to ~25s on
// the device-grant flow, answering 200 the moment the human approves, or 204 on
// hold-timeout while still pending. The CLI re-polls immediately on 204 (the
// server already absorbed the wait), so the human-visible latency is ~ms after
// the Approve click rather than a fixed poll interval.
//
// The CLI prints verification_url + user_code, the developer approves in a
// browser already authenticated by the magic-link session, and the CLI polls
// until the bearer is minted.

// defaultPollTimeout bounds a single poll request. It sits ABOVE the api's ≤25s
// hold so a healthy long-poll never trips it; if it does fire mid-hold we treat
// it as "re-poll", not a fatal error (see PollUntilToken). A Client may override
// it via PollTimeout (zero ⇒ this default); production leaves it at 35s.
const defaultPollTimeout = 35 * time.Second

type DeviceStart struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	// IntervalSeconds is the server's suggested cadence. With long-polling the
	// 25s hold IS the pacing, so this is no longer the primary cadence — it's
	// only a defensive floor / fallback against a server that returns pending
	// instantly in a loop (see PollUntilToken's backoff).
	IntervalSeconds int `json:"interval"`
}

// DeviceToken is the poll's approved-grant response. It carries the minted
// bearer only — the CLI session proves identity; context is resolved per
// command, so the poll no longer returns a context.
type DeviceToken struct {
	Token string `json:"token"`
}

// ErrAuthPending is the poll's "keep waiting, re-poll" signal: the server
// answered 204 on its hold-timeout while the grant is still pending (also 202,
// for resilience), OR this single poll's own ~35s HTTP timeout fired mid-hold.
// Either way the caller should re-poll; it is NOT a terminal error.
var ErrAuthPending = errors.New("authorization pending")

func (c *Client) DeviceStart(ctx context.Context) (*DeviceStart, error) {
	var out DeviceStart
	if err := c.do(ctx, http.MethodPost, "/api/login/device/start", map[string]string{}, &out); err != nil {
		return nil, err
	}
	if out.IntervalSeconds <= 0 {
		out.IntervalSeconds = 5
	}
	return &out, nil
}

// DevicePoll issues ONE long-poll. The server holds the request up to ~25s and
// answers:
//   - 200 {token} → approved; returns the minted bearer.
//   - 204 (also 202, for resilience) → still pending on hold-timeout; returns
//     ErrAuthPending so the caller re-polls immediately.
//   - 4xx/5xx → terminal *APIError (e.g. expired_token / access_denied).
//
// It derives its own ~35s timeout (above the hold) from the caller ctx so a
// healthy hold never trips it. If that per-request deadline DOES fire while the
// caller ctx is still live, it's a long-poll that timed out client-side mid-
// hold — returned as ErrAuthPending (re-poll), not a fatal error. Cancellation
// of the caller ctx itself (the outer login deadline) surfaces as its ctx.Err.
func (c *Client) DevicePoll(ctx context.Context, deviceCode string) (*DeviceToken, error) {
	pollTimeout := c.PollTimeout
	if pollTimeout <= 0 {
		pollTimeout = defaultPollTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()

	b, _ := json.Marshal(map[string]string{"device_code": deviceCode})
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		c.BaseURL+"/api/login/device/poll", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// This poll's own deadline fired mid-hold but the outer login ctx is
		// still live → just re-poll. The outer ctx being done is terminal.
		if reqCtx.Err() != nil && ctx.Err() == nil {
			return nil, ErrAuthPending
		}
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusNoContent, resp.StatusCode == http.StatusAccepted:
		return nil, ErrAuthPending
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, &APIError{Status: resp.StatusCode, Body: string(raw)}
	}
	var out DeviceToken
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// minRepollInterval is the defensive floor between polls. With long-polling
// each poll normally blocks ~25s on the server's hold, so we re-poll the moment
// one returns pending — no sleep. This floor only guards against a MISBEHAVING
// server that answers 204 instantly in a tight loop: if a poll returned pending
// in well under the hold time, we back off this long before re-polling.
const minRepollInterval = time.Second

// PollUntilToken long-polls until approval, the outer ctx's deadline/cancel, or
// a terminal error. The server's ≤25s hold is the pacing, so on ErrAuthPending
// it re-polls immediately — UNLESS the poll returned pending suspiciously fast
// (a server not honouring the hold), in which case it applies minRepollInterval
// as a backoff so a broken server can't be hammered.
func (c *Client) PollUntilToken(ctx context.Context, start *DeviceStart) (*DeviceToken, error) {
	floor := time.Duration(start.IntervalSeconds) * time.Second
	if floor <= 0 || floor > minRepollInterval {
		// The defensive floor is small regardless of the server-suggested
		// interval; the long-poll hold provides the real pacing.
		floor = minRepollInterval
	}
	for {
		began := time.Now()
		tok, err := c.DevicePoll(ctx, start.DeviceCode)
		if err == nil {
			return tok, nil
		}
		if !errors.Is(err, ErrAuthPending) {
			return nil, err
		}
		// Re-poll immediately when the poll consumed (roughly) the server's
		// hold; only back off if it returned pending implausibly fast.
		if wait := floor - time.Since(began); wait > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		} else if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
}
