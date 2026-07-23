// Package httpx is the shared HTTP base for the fixed-labs CLIs: a small
// bearer-authenticated JSON client plus the typed APIError both rift and fplctl
// branch on. Stdlib only. Each CLI's own client.Client embeds *httpx.Client and
// adds its endpoint methods, calling Do.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// PollTimeout bounds a single device-flow poll request (see
	// deviceflow.Poll). Zero means the production default; tests inject a small
	// value to exercise the per-request-timeout branch without a real wait.
	PollTimeout time.Duration
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: baseURL,
		Token:   token,
		// 35s is a PER-request cap, set above the login endpoint's ≤25s
		// long-poll hold so a healthy hold never trips it.
		HTTP: &http.Client{Timeout: 35 * time.Second},
	}
}

// APIError carries a non-2xx response so callers can branch on status.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// Do issues one bearer-authenticated JSON request: body (if non-nil) is
// marshaled as the request JSON; out (if non-nil) is unmarshaled from a 2xx
// body. A non-2xx response returns an *APIError.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Status: resp.StatusCode, Body: string(raw)}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return nil
}
