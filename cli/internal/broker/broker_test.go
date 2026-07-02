package broker

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/fixed-labs/oss/cli/internal/secrets"
)

// newTestUserConfig writes a user config that maps std:aws → an AWS creds INI
// file and std:npm → a token file, and returns the loaded config plus the repo
// dir to resolve against.
func newTestUserConfig(t *testing.T) (*secrets.UserConfig, string) {
	t.Helper()
	dir := t.TempDir()

	awsFile := filepath.Join(dir, "aws-creds")
	if err := os.WriteFile(awsFile, []byte("[default]\naws_access_key_id = AKIATEST\naws_secret_access_key = shhh\naws_session_token = tok123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	npmFile := filepath.Join(dir, "npm-token")
	if err := os.WriteFile(npmFile, []byte("npm-secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ucPath := filepath.Join(dir, "secrets.json")
	uc, err := secrets.LoadUserConfig(ucPath)
	if err != nil {
		t.Fatal(err)
	}
	uc.Defaults["aws"] = secrets.Source{Path: awsFile}
	uc.Defaults["npm"] = secrets.Source{Path: npmFile}
	if err := uc.Save(); err != nil {
		t.Fatal(err)
	}
	return uc, "someowner/somerepo"
}

func TestStaticProviderInjectAWS(t *testing.T) {
	uc, repo := newTestUserConfig(t)
	p := NewStaticProvider(uc, repo)
	cred, err := p.Mint(context.Background(), MintRequest{Secret: "aws"})
	if err != nil {
		t.Fatalf("Mint aws: %v", err)
	}
	if cred.Kind != KeyTriple {
		t.Fatalf("kind = %q, want %q", cred.Kind, KeyTriple)
	}
	want := map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKIATEST",
		"AWS_SECRET_ACCESS_KEY": "shhh",
		"AWS_SESSION_TOKEN":     "tok123",
	}
	got := map[string]string{}
	for _, p := range cred.Env {
		got[p.Name] = p.Value
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("env %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestStaticProviderInjectNPM(t *testing.T) {
	uc, repo := newTestUserConfig(t)
	p := NewStaticProvider(uc, repo)
	cred, err := p.Mint(context.Background(), MintRequest{Secret: "npm"})
	if err != nil {
		t.Fatalf("Mint npm: %v", err)
	}
	if cred.Kind != BearerToken {
		t.Fatalf("kind = %q, want %q", cred.Kind, BearerToken)
	}
	if len(cred.Env) != 1 || cred.Env[0].Name != "NPM_TOKEN" || cred.Env[0].Value != "npm-secret-token" {
		t.Fatalf("npm env = %+v", cred.Env)
	}
	// Materialize bytes preserve the raw source (trailing newline included).
	if string(cred.Raw) != "npm-secret-token\n" {
		t.Fatalf("raw = %q", cred.Raw)
	}
}

func TestStaticProviderUnknownSecret(t *testing.T) {
	uc, repo := newTestUserConfig(t)
	p := NewStaticProvider(uc, repo)
	_, err := p.Mint(context.Background(), MintRequest{Secret: "nope-not-a-secret"})
	if err == nil {
		t.Fatal("expected error for unknown secret")
	}
	var re *resolveError
	if !errors.As(err, &re) {
		t.Fatalf("expected resolveError, got %T: %v", err, err)
	}
}

func TestStaticProviderUnmappedSecret(t *testing.T) {
	// claude is a real inject secret but has no source mapped here.
	uc, repo := newTestUserConfig(t)
	p := NewStaticProvider(uc, repo)
	_, err := p.Mint(context.Background(), MintRequest{Secret: "claude"})
	if err == nil {
		t.Fatal("expected error for unmapped secret")
	}
	var re *resolveError
	if !errors.As(err, &re) {
		t.Fatalf("expected resolveError, got %T: %v", err, err)
	}
}

// runHandler spins the handler on a loopback listener and returns a Client wired
// to dial it via an injected dialer. This exercises the full wire round-trip
// without a tunnel or box.
func runHandler(t *testing.T, uc *secrets.UserConfig, repo string) *Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	h := NewHandler(NewStaticProvider(uc, repo), nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = h.Serve(ctx, ln) }()
	addr := ln.Addr().String()
	return &Client{Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}}
}

func TestWireRoundTripInject(t *testing.T) {
	uc, repo := newTestUserConfig(t)
	c := runHandler(t, uc, repo)
	pairs, err := c.FetchInject(context.Background(), "aws")
	if err != nil {
		t.Fatalf("FetchInject: %v", err)
	}
	got := map[string]string{}
	for _, p := range pairs {
		got[p.Name] = p.Value
	}
	if got["AWS_ACCESS_KEY_ID"] != "AKIATEST" || got["AWS_SECRET_ACCESS_KEY"] != "shhh" || got["AWS_SESSION_TOKEN"] != "tok123" {
		t.Fatalf("env over wire = %+v", got)
	}
}

func TestWireRoundTripMaterialize(t *testing.T) {
	uc, repo := newTestUserConfig(t)
	c := runHandler(t, uc, repo)
	raw, err := c.FetchMaterialize(context.Background(), "npm")
	if err != nil {
		t.Fatalf("FetchMaterialize: %v", err)
	}
	if string(raw) != "npm-secret-token\n" {
		t.Fatalf("materialize bytes = %q", raw)
	}
}

func TestWireRoundTripUnknownSecret(t *testing.T) {
	uc, repo := newTestUserConfig(t)
	c := runHandler(t, uc, repo)
	_, err := c.FetchInject(context.Background(), "bogus")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrUnknownSecretRemote) {
		t.Fatalf("want ErrUnknownSecretRemote, got %v", err)
	}
	// The error detail must not contain a credential value; it names only the
	// secret. (Sanity: nothing in the message looks like our test secret bytes.)
	if msg := err.Error(); contains(msg, "shhh") || contains(msg, "AKIATEST") || contains(msg, "npm-secret-token") {
		t.Fatalf("error leaked a credential value: %q", msg)
	}
}

func TestClientUnreachable(t *testing.T) {
	// A dialer that always fails models a downed laptop session.
	c := &Client{Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}}
	_, err := c.FetchInject(context.Background(), "aws")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
}

func TestHandleBadRequest(t *testing.T) {
	uc, repo := newTestUserConfig(t)
	h := NewHandler(NewStaticProvider(uc, repo), nil)
	if r := h.Handle(context.Background(), Request{Secret: "", Mode: ModeInject}); r.Error != ErrBadRequest {
		t.Fatalf("empty secret: got %q", r.Error)
	}
	if r := h.Handle(context.Background(), Request{Secret: "aws", Mode: "weird"}); r.Error != ErrBadRequest {
		t.Fatalf("bad mode: got %q", r.Error)
	}
}

func TestParseLaptopIPs(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
		wantIs  error
	}{
		{name: "single ipv6", in: "fd5e:de7b::af8\n", want: "fd5e:de7b::af8"},
		{name: "ipv4", in: "100.64.0.2\n", want: "100.64.0.2"},
		{name: "empty (no connection attached)", in: "", wantErr: true, wantIs: ErrUnreachable},
		{name: "blank lines only", in: "\n   \n", wantErr: true, wantIs: ErrUnreachable},
		{name: "two ips — ambiguous", in: "fd5e::1\nfd5e::2\n", wantErr: true, wantIs: ErrMultipleConnections},
		{name: "surrounding whitespace tolerated", in: "  fd5e::9  \n\n", want: "fd5e::9"},
		{name: "non-ip line ignored", in: "garbage\nfd5e::9\n", want: "fd5e::9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLaptopIPs(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
					t.Fatalf("want errors.Is(_, %v), got %v", tc.wantIs, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDiscoverLaptopIPFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "laptop-wg-ips")
	orig := LaptopIPFile
	LaptopIPFile = path
	defer func() { LaptopIPFile = orig }()

	// Missing file → ErrUnreachable (no laptop attached → "session down"), NOT a
	// silent fall-through.
	if _, err := DiscoverLaptopIP(); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("missing file: want ErrUnreachable, got %v", err)
	}
	// One published IP → returned.
	if err := os.WriteFile(path, []byte("fd5e:de7b::af8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverLaptopIP()
	if err != nil {
		t.Fatalf("DiscoverLaptopIP: %v", err)
	}
	if got != "fd5e:de7b::af8" {
		t.Fatalf("got %q", got)
	}
	// Two IPs (a stale connection not cleaned up) → ErrMultipleConnections,
	// distinct from the unreachable/session-down case.
	if err := os.WriteFile(path, []byte("fd5e::1\nfd5e::2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverLaptopIP(); !errors.Is(err, ErrMultipleConnections) {
		t.Fatalf("two IPs: want ErrMultipleConnections, got %v", err)
	}
	// Empty file (connection detached → agent truncated it) → ErrUnreachable.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverLaptopIP(); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("empty file: want ErrUnreachable, got %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
