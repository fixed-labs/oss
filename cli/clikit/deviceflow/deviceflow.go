// Package deviceflow implements the OAuth-device-grant login flow shared by the
// fixed-labs CLIs, against the server's /api/login/device endpoints:
//
//	POST /api/login/device/start              → {device_code, user_code, verification_url, interval}
//	POST /api/login/device/poll {device_code} → 200 {token} | 204/202 (pending) | 4xx (terminal)
//
// The poll is a server-side LONG-poll: the api holds each request up to ~25s,
// answering 200 the moment the human approves, or 204 on hold-timeout while
// still pending. The CLI re-polls immediately on 204.
package deviceflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/fixed-labs/oss/cli/clikit/httpx"
)

// defaultPollTimeout bounds a single poll request. It sits ABOVE the api's ≤25s
// hold so a healthy long-poll never trips it; if it fires mid-hold we treat it
// as "re-poll", not fatal. Overridable via httpx.Client.PollTimeout.
const defaultPollTimeout = 35 * time.Second

type DeviceStart struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	IntervalSeconds int    `json:"interval"`
}

// DeviceToken is the poll's approved-grant response — the minted bearer only.
type DeviceToken struct {
	Token string `json:"token"`
}

// ErrAuthPending is the poll's "keep waiting, re-poll" signal: the server
// answered 204/202 on its hold-timeout while still pending, OR this poll's own
// ~35s HTTP timeout fired mid-hold. Either way, re-poll; NOT terminal.
var ErrAuthPending = errors.New("authorization pending")

func Start(ctx context.Context, c *httpx.Client) (*DeviceStart, error) {
	var out DeviceStart
	if err := c.Do(ctx, http.MethodPost, "/api/login/device/start", map[string]string{}, &out); err != nil {
		return nil, err
	}
	if out.IntervalSeconds <= 0 {
		out.IntervalSeconds = 5
	}
	return &out, nil
}

// Poll issues ONE long-poll. 200 {token} → approved; 204/202 → ErrAuthPending
// (re-poll); 4xx/5xx → terminal *httpx.APIError. It derives its own ~35s
// timeout (above the hold) from ctx; if that fires while ctx is still live it's
// a mid-hold client timeout → ErrAuthPending.
func Poll(ctx context.Context, c *httpx.Client, deviceCode string) (*DeviceToken, error) {
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
		// This poll's deadline fired mid-hold but the outer ctx is still live →
		// re-poll. The outer ctx being done is terminal.
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
		return nil, &httpx.APIError{Status: resp.StatusCode, Body: string(raw)}
	}
	var out DeviceToken
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// minRepollInterval is the defensive floor between polls, guarding against a
// misbehaving server that answers 204 instantly in a tight loop.
const minRepollInterval = time.Second

// PollUntilToken long-polls until approval, the outer ctx's deadline/cancel, or
// a terminal error. On ErrAuthPending it re-polls immediately unless the poll
// returned pending implausibly fast, in which case it backs off minRepollInterval.
func PollUntilToken(ctx context.Context, c *httpx.Client, start *DeviceStart) (*DeviceToken, error) {
	floor := time.Duration(start.IntervalSeconds) * time.Second
	if floor <= 0 || floor > minRepollInterval {
		floor = minRepollInterval
	}
	for {
		began := time.Now()
		tok, err := Poll(ctx, c, start.DeviceCode)
		if err == nil {
			return tok, nil
		}
		if !errors.Is(err, ErrAuthPending) {
			return nil, err
		}
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
