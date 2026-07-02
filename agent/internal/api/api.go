// Package api is the agent's HTTP client for the control-plane's in-VM agent
// HTTP surface:
//
//	POST /api/rift/v1/{ws}/events        — cloning | failed
//	POST /api/rift/v1/{ws}/heartbeat     — liveness + interactive (SSH/session) liveness + identity
//	                                       (identity drives provisioned/starting → running)
//	GET  /api/rift/v1/{ws}/agent-config  — the PULLED desired state
//	     (?cursor= long-poll: 200 {cursor, peers} on change, 304 on timeout)
//
// All requests carry the workspace-scoped bearer. The agent never sends a
// timestamp — the control plane stamps each write server-side.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Peer is one authorized laptop from the pulled config — simultaneously a
// WireGuard peer of wg0 and a row in the SSH server's source-wg-ip →
// identity table (one config, two uses).
type Peer struct {
	LaptopWgPubkey string `json:"laptop_wg_pubkey"`
	LaptopWgIP     string `json:"laptop_wg_ip"`
	DeveloperID    string `json:"developer_id"`
	LoginUser      string `json:"login_user"`
	RelayEndpoint  string `json:"relay_endpoint"`
	RelayPort      int    `json:"relay_port"`
	LeaseUntil     int64  `json:"lease_until"`
}

// Config is one agent-config pull result.
type Config struct {
	Cursor string `json:"cursor"`
	Peers  []Peer `json:"peers"`
}

// ErrNotModified is returned by PullConfig when the server answers 304 (no
// change within the hold window) — re-poll with the same cursor.
var ErrNotModified = fmt.Errorf("agent config not modified")

type Client struct {
	BaseURL     string
	WorkspaceID string
	Token       string
	// HTTP must outlive the server's ≤25s long-poll hold.
	HTTP *http.Client
}

func New(baseURL, workspaceID, token string) *Client {
	return &Client{
		BaseURL:     baseURL,
		WorkspaceID: workspaceID,
		Token:       token,
		HTTP:        &http.Client{Timeout: 35 * time.Second},
	}
}

func (c *Client) url(leaf string) string {
	return fmt.Sprintf("%s/api/rift/v1/%s/%s", c.BaseURL, c.WorkspaceID, leaf)
}

func (c *Client) post(ctx context.Context, leaf string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(leaf), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("POST %s: HTTP %d: %s", leaf, resp.StatusCode, string(b))
	}
	return nil
}

// Identity is the machine's VM-self-generated PUBLIC identity (the private keys
// never leave the volume). It rides EVERY heartbeat: the cluster persists it and
// flips provisioned/starting → running off a non-empty WgPubkey. There is no
// one-shot ready report — readiness is a continuously re-asserted fact.
type Identity struct {
	SSHHost        string
	EditorURL      string
	ResolvedCommit string
	WgPubkey       string
	SSHHostPubkey  string
}

// ReportCloning reports the cloning lifecycle event (image baking flows that
// still clone at boot; most client images carry the code and skip this).
func (c *Client) ReportCloning(ctx context.Context) error {
	return c.post(ctx, "events", map[string]any{"type": "cloning"})
}

// ReportFailed reports a fatal boot failure (the cluster grants an operator
// inspection window, then reaps).
func (c *Client) ReportFailed(ctx context.Context, errorMessage string) error {
	return c.post(ctx, "events", map[string]any{
		"type":          "failed",
		"error_message": errorMessage,
	})
}

// Heartbeat reports liveness + identity. interactiveLive is the supervisor's
// folded interactive-liveness flag (open SSH connections OR held/attached
// persistent sessions) and pushes back the idle-suspend clock; sshSessions rides
// along as the raw count (diagnostic; the api re-folds it defensively). The
// identity facts drive the cluster's provisioned/starting → running flip (no
// separate ready event).
func (c *Client) Heartbeat(ctx context.Context, interactiveLive bool, sshSessions int, id Identity) error {
	return c.post(ctx, "heartbeat", map[string]any{
		"interactive_live": interactiveLive,
		"ssh_sessions":     sshSessions,
		"ssh_host":         id.SSHHost,
		"editor_url":       id.EditorURL,
		"resolved_commit":  id.ResolvedCommit,
		"wg_pubkey":        id.WgPubkey,
		"ssh_host_pubkey":  id.SSHHostPubkey,
	})
}

// SessionMeta is one session's entry in a SyncSessions snapshot — the box-side
// metadata the control plane projects (no terminal bytes ever leave the box).
type SessionMeta struct {
	Name          string `json:"name"`
	AttachedCount int64  `json:"attached_count"`
	ForegroundCmd string `json:"foreground_cmd"`
	ForegroundCwd string `json:"foreground_cwd"`
}

// CreateSession reports a newly-minted session (the `create` message). It is
// the read-after-write barrier: the POST blocks until the new session is
// durably applied and replicated server-side, so the CLI's
// immediately-following get-sessions observes the new row. The agent sends NO
// timestamp — the control plane stamps the write server-side.
func (c *Client) CreateSession(ctx context.Context, id, name string, genEpoch int64) error {
	return c.post(ctx, "sessions", map[string]any{
		"type":      "create",
		"id":        id,
		"name":      name,
		"gen_epoch": genEpoch,
	})
}

// EndSession reports a session whose root shell exited (the `end` message) —
// the server drops the session id.
func (c *Client) EndSession(ctx context.Context, id, reason string) error {
	return c.post(ctx, "sessions", map[string]any{
		"type":   "end",
		"id":     id,
		"reason": reason,
	})
}

// SyncSessions reports a full snapshot of all live sessions (the `sync` message):
// the periodic (heartbeat-cadence) + on-attach/detach upsert. Self-healing — a
// row missing because a create append was dropped is restored; never deletes.
func (c *Client) SyncSessions(ctx context.Context, genEpoch int64, sessions map[string]SessionMeta) error {
	return c.post(ctx, "sessions", map[string]any{
		"type":      "sync",
		"gen_epoch": genEpoch,
		"sessions":  sessions,
	})
}

// TombstoneStaleSessions removes every session row with gen-epoch < genEpoch
// (the `tombstone` message) — the boot-reconcile call on a fresh process.
func (c *Client) TombstoneStaleSessions(ctx context.Context, genEpoch int64) error {
	return c.post(ctx, "sessions", map[string]any{
		"type":      "tombstone",
		"gen_epoch": genEpoch,
	})
}

// PullConfig long-polls the agent config. cursor "" = absolute read (returns
// immediately with the current peer set); otherwise holds server-side ≤25s
// and returns ErrNotModified on 304.
func (c *Client) PullConfig(ctx context.Context, cursor string) (*Config, error) {
	u := c.url("agent-config")
	if cursor != "" {
		u += "?cursor=" + cursor
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return nil, ErrNotModified
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("GET agent-config: HTTP %d: %s", resp.StatusCode, string(b))
	}
	var cfg Config
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode agent-config: %w", err)
	}
	return &cfg, nil
}
