package sessions

// T8b(a)/(e) — banner delivery through the REAL persistent-session surface.
// The bootstrap fallback-path NS fixture (Bootstrap true, sd-pid nonexistent)
// makes Route return a runnable local Legacy shell + banner — no root/nsenter
// needed. The shell is a deterministic-emit script so ring ordering (banner
// seeded BEFORE the readLoop's output) is assertable, and the legacy sibling
// shares the same script fixture varying ONLY the NS axis.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixed-labs/oss/agent/internal/nsexec"
)

// The no-client banner's identifying token (§3.6). The full prose is
// incidental; this token cannot occur in the shell fixture's output.
const tokNoClient = "client system not running"

const shellSentinel = "RIFT_T8B_SHELL_OUTPUT_1"

// deterministicShell writes a shell whose output is exactly one known line,
// then sleeps (session stays alive for a later attach, no further output).
func deterministicShell(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "shell.sh")
	script := "#!/bin/sh\nprintf '" + shellSentinel + "\\r\\n'\nexec sleep 30\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// fallbackNS is the bootstrap fallback-path fixture: sd-pid path nonexistent,
// so Route returns the Legacy shell plus the no-client banner.
func fallbackNS(t *testing.T) *nsexec.NS {
	t.Helper()
	miss := filepath.Join(t.TempDir(), "absent")
	return &nsexec.NS{
		Bootstrap:        true,
		SDPidFile:        filepath.Join(miss, "sd-pid"),
		RecoveryBootFile: filepath.Join(miss, "recovery-boot"),
		BootRungFile:     filepath.Join(miss, "boot-rung"),
	}
}

// specCapture records every Spec Route receives (seam vi).
type specCapture struct {
	mu    sync.Mutex
	specs []nsexec.Spec
}

func captureRouteSpecs(t *testing.T) *specCapture {
	t.Helper()
	c := &specCapture{}
	nsexec.SetRouteSpecObserver(func(s nsexec.Spec) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.specs = append(c.specs, s)
	})
	t.Cleanup(func() { nsexec.SetRouteSpecObserver(nil) })
	return c
}

func (c *specCapture) all() []nsexec.Spec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]nsexec.Spec(nil), c.specs...)
}

// drainBufferedFrames pops every frame currently buffered on the client's
// queue without blocking.
func drainBufferedFrames(c *Client) [][]byte {
	var frames [][]byte
	for {
		select {
		case f := <-c.out:
			frames = append(frames, f)
		default:
			return frames
		}
	}
}

func TestBootstrapFallbackBannerSeededBeforeShellOutput(t *testing.T) {
	rc := captureRouteSpecs(t)
	m := NewManager(Config{
		Shell:    deterministicShell(t),
		Home:     t.TempDir(),
		Login:    "rift-login-sentinel-a",
		NS:       fallbackNS(t),
		API:      &fakeAPI{},
		GenEpoch: 5,
	})
	c1 := NewClient(80, 24)
	s, err := m.CreateOrAttachDefault(c1)
	if err != nil {
		t.Fatal(err)
	}
	b1 := drainClient(c1)
	// The creating attach already replays the seeded banner, then the live
	// shell output follows.
	waitFor(t, 5*time.Second, func() bool {
		out := b1.String()
		return strings.Contains(out, tokNoClient) && strings.Contains(out, shellSentinel)
	})

	// A LATER attach must replay the banner ahead of the shell output — the
	// ring-seed ordering (§3.6): the banner went in before the readLoop
	// started, so the replay reconstructs banner-then-output.
	c2 := NewClient(80, 24)
	if _, err := m.Attach(s.ID(), c2); err != nil {
		t.Fatal(err)
	}
	frames := drainBufferedFrames(c2)
	if len(frames) != 2 {
		t.Fatalf("attach buffered %d frames, want 2 (clear+home, replay): %q", len(frames), frames)
	}
	if string(frames[0]) != "\x1b[2J\x1b[H" {
		t.Fatalf("frame 0 = %q, want the leading clear+home", frames[0])
	}
	replay := string(frames[1])
	idxBanner := strings.Index(replay, tokNoClient)
	idxOut := strings.Index(replay, shellSentinel)
	if idxBanner < 0 || idxOut < 0 {
		t.Fatalf("replay missing banner (%d) or shell output (%d): %q", idxBanner, idxOut, replay)
	}
	if idxBanner > idxOut {
		t.Fatalf("banner replayed AFTER shell output (banner %d, output %d): %q", idxBanner, idxOut, replay)
	}

	// Seam (vi): what startShell passed to Route — the resolved login user, an
	// empty Command (interactive shell), and the TERM entry (`runuser -l`
	// preserves only TERM, so this one entry is the routed-PTY contract).
	specs := rc.all()
	if len(specs) == 0 {
		t.Fatal("Route observer captured nothing")
	}
	sp := specs[len(specs)-1]
	if sp.Login != "rift-login-sentinel-a" {
		t.Fatalf("Spec.Login = %q, want the Manager's resolved login", sp.Login)
	}
	if sp.Command != "" {
		t.Fatalf("Spec.Command = %q, want empty for an interactive shell", sp.Command)
	}
	if len(sp.Env) != 1 || sp.Env[0] != "TERM=xterm-256color" {
		t.Fatalf("Spec.Env = %q, want exactly [TERM=xterm-256color]", sp.Env)
	}

	s.Detach(c1)
	s.Detach(c2)
}

// The legacy sibling (T8b(e), sessions half): the SAME script fixture with the
// NS axis nil — output reaches the client with the sentinel and NO banner
// (nil-NS handling must not inject spurious banner writes).
func TestLegacyManagerStreamCarriesNoBanner(t *testing.T) {
	m := NewManager(Config{
		Shell:    deterministicShell(t),
		Home:     t.TempDir(),
		Login:    "rift-login-sentinel-a",
		API:      &fakeAPI{},
		GenEpoch: 5,
	})
	c1 := NewClient(80, 24)
	s, err := m.CreateOrAttachDefault(c1)
	if err != nil {
		t.Fatal(err)
	}
	b1 := drainClient(c1)
	// Positive anchor: the shell's output arrived.
	waitFor(t, 5*time.Second, func() bool { return strings.Contains(b1.String(), shellSentinel) })
	if out := b1.String(); strings.Contains(out, tokNoClient) || strings.Contains(out, "recovery boot") {
		t.Fatalf("legacy stream carries a banner: %q", out)
	}
	// A later attach's replay is the shell output alone.
	c2 := NewClient(80, 24)
	if _, err := m.Attach(s.ID(), c2); err != nil {
		t.Fatal(err)
	}
	frames := drainBufferedFrames(c2)
	if len(frames) != 2 {
		t.Fatalf("attach buffered %d frames, want 2: %q", len(frames), frames)
	}
	replay := string(frames[1])
	if !strings.Contains(replay, shellSentinel) {
		t.Fatalf("replay missing the shell output: %q", replay)
	}
	if strings.Contains(replay, tokNoClient) {
		t.Fatalf("legacy replay carries a banner: %q", replay)
	}
	s.Detach(c1)
	s.Detach(c2)
}
