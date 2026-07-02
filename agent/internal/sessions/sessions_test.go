package sessions

import (
	"context"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixed-labs/oss/agent/internal/api"
)

// fakeAPI records every session POST. Safe for concurrent use.
type fakeAPI struct {
	mu         sync.Mutex
	creates    []string // session ids
	ends       []string // session ids
	syncs      []map[string]api.SessionMeta
	tombstones []int64
}

func (f *fakeAPI) CreateSession(_ context.Context, id, _ string, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, id)
	return nil
}
func (f *fakeAPI) EndSession(_ context.Context, id, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ends = append(f.ends, id)
	return nil
}
func (f *fakeAPI) SyncSessions(_ context.Context, _ int64, snap map[string]api.SessionMeta) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs = append(f.syncs, snap)
	return nil
}
func (f *fakeAPI) TombstoneStaleSessions(_ context.Context, ge int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tombstones = append(f.tombstones, ge)
	return nil
}

func (f *fakeAPI) endCount() int    { f.mu.Lock(); defer f.mu.Unlock(); return len(f.ends) }
func (f *fakeAPI) createCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.creates) }

// testManager builds a Manager whose shells are an interactive bash/sh as the
// current user (no setuid in tests).
func testManager(t *testing.T, api SessionAPI) *Manager {
	t.Helper()
	shell := "/bin/bash"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}
	u, _ := user.Current()
	home := t.TempDir()
	if u != nil && u.HomeDir != "" {
		home = u.HomeDir
	}
	return NewManager(Config{Shell: shell, Home: home, API: api, GenEpoch: 5})
}

// safeBuf is a concurrency-safe accumulator: the drain goroutine appends while
// waitFor reads, so the buffer needs its own lock (the production fan-out copies
// bytes before enqueue; this is purely a test-harness concern).
type safeBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuf) Write(p []byte) {
	s.mu.Lock()
	s.b.Write(p)
	s.mu.Unlock()
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// drainClient reads everything a client emits until its queue closes,
// accumulating into a concurrency-safe buffer. Runs in its own goroutine like
// the real per-connection writer.
func drainClient(c *Client) *safeBuf {
	b := &safeBuf{}
	go func() {
		for chunk := range c.Output() {
			b.Write(chunk)
		}
	}()
	return b
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within " + d.String())
}

func TestDefaultSingleCreatesMain(t *testing.T) {
	api := &fakeAPI{}
	m := testManager(t, api)
	c := NewClient(80, 24)
	s, err := m.CreateOrAttachDefault(c)
	if err != nil {
		t.Fatalf("CreateOrAttachDefault: %v", err)
	}
	if s.Name() != "main" {
		t.Fatalf("default session name = %q, want main", s.Name())
	}
	if s.GenEpoch() != 5 {
		t.Fatalf("session gen-epoch = %d, want 5 (the manager's bumped epoch)", s.GenEpoch())
	}
	if m.HeldLivePTYs() != 1 || m.AttachedClients() != 1 {
		t.Fatalf("held=%d attached=%d", m.HeldLivePTYs(), m.AttachedClients())
	}
	waitFor(t, 2*time.Second, func() bool { return api.createCount() == 1 })
	s.Detach(c)
}

// TestConcurrentFirstConnectsShareMain proves the single-mutex name allocation:
// two concurrent first-connects must land on ONE `main`.
func TestConcurrentFirstConnectsShareMain(t *testing.T) {
	m := testManager(t, &fakeAPI{})
	var wg sync.WaitGroup
	ids := make([]string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := NewClient(80, 24)
			s, err := m.CreateOrAttachDefault(c)
			if err != nil {
				t.Errorf("attach %d: %v", i, err)
				return
			}
			ids[i] = s.ID()
		}(i)
	}
	wg.Wait()
	if ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("concurrent first-connects created different sessions: %v", ids)
	}
	if m.HeldLivePTYs() != 1 {
		t.Fatalf("HeldLivePTYs = %d, want 1", m.HeldLivePTYs())
	}
}

// TestMirroredAttach: two clients on one session both see output and both can
// type (the kernel serializes writes to the master).
func TestMirroredAttach(t *testing.T) {
	m := testManager(t, &fakeAPI{})
	c1 := NewClient(80, 24)
	s, err := m.CreateOrAttachDefault(c1)
	if err != nil {
		t.Fatal(err)
	}
	c2 := NewClient(80, 24)
	if _, err := m.Attach(s.ID(), c2); err != nil {
		t.Fatal(err)
	}
	b1 := drainClient(c1)
	b2 := drainClient(c2)

	// Type a marker via the PTY and confirm both mirrors see the echo/output.
	if _, err := s.Write([]byte("echo MIRROR_MARKER_XYZ\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(b1.String(), "MIRROR_MARKER_XYZ") &&
			strings.Contains(b2.String(), "MIRROR_MARKER_XYZ")
	})
	s.Detach(c1)
	s.Detach(c2)
}

// TestAttachSignalsRepaint: attaching delivers SIGWINCH to the session's
// foreground process group so the shell/app repaints onto the freshly-cleared
// screen — even when the new client's geometry matches the held size (the
// reconnect case, where pty.Setsize sends no SIGWINCH because nothing changed).
// Regression guard for the "blank screen until a manual resize on reconnect" bug.
// TestReplaySurvivesReattach guards the reconnect-shows-blank regression: the
// ring replay reconstructs the VISIBLE screen (a shell's `ls` output lives there,
// not in off-screen scrollback), so attach() must buffer a leading clear+home and
// the replay — and NOTHING after it. A trailing clear+home wipes the reconstructed
// frame, and a line-based shell never repaints scrolled content on the SIGWINCH
// nudge, so the user would reconnect to a blank screen.
//
// Driven deterministically: a minimal Session with winW/winH 0 (via NewClient(0,0)
// → recomputeWin keeps them 0) makes attach() skip pty.Setsize, and pgid 0 makes
// it skip the SIGWINCH kill — so only the replay buffering runs, no live PTY.
func TestReplaySurvivesReattach(t *testing.T) {
	s := &Session{ring: newRing(4096), clients: map[*Client]struct{}{}}
	s.ring.append([]byte("PROMPT$ ls\r\nfile_a file_b\r\nPROMPT$ "))

	c := NewClient(0, 0)
	s.attach(c)

	var frames [][]byte
	for {
		select {
		case f := <-c.out:
			frames = append(frames, f)
			continue
		default:
		}
		break
	}
	// Exactly two frames: leading clear+home, then the replay. A trailing clear
	// (the regression) would make it three and wipe the replayed screen.
	if len(frames) != 2 {
		t.Fatalf("attach buffered %d frames, want exactly 2 (clear+home, replay); a "+
			"trailing clear that wipes the replay makes it 3: %q", len(frames), frames)
	}
	if string(frames[0]) != "\x1b[2J\x1b[H" {
		t.Fatalf("frame 0 = %q, want a leading clear+home", frames[0])
	}
	if !strings.Contains(string(frames[1]), "file_a file_b") {
		t.Fatalf("replay frame missing the on-screen output: %q", frames[1])
	}
}

func TestAttachSignalsRepaint(t *testing.T) {
	// Requires a native bash whose readline redraws the idle prompt on a no-op
	// (same-geometry) SIGWINCH. Skip when /bin/bash is absent OR is a wrapper
	// SCRIPT rather than a native ELF binary — e.g. the Nix-store bash shim used
	// in the CI container: its readline does NOT redraw on a no-op SIGWINCH, so
	// the condition below never fires. This is the "NixOS bash lives in the Nix
	// store" case the guard was always meant to cover; a bare existence check is
	// fooled by the shim sitting at /bin/bash.
	shell := "/bin/bash"
	f, err := os.Open(shell)
	if err != nil {
		t.Skipf("skipping: requires bash at /bin/bash (SIGWINCH→readline repaint); found: %v", err)
	}
	magic := make([]byte, 4)
	n, _ := io.ReadFull(f, magic)
	f.Close()
	if n < 4 || string(magic) != "\x7fELF" {
		t.Skip("skipping: /bin/bash is a wrapper script, not a native ELF bash (e.g. the Nix-store shim in CI); its readline does not repaint on a no-op SIGWINCH")
	}
	// Clean HOME so the test bash isn't perturbed by the dev's dotfiles.
	dir := t.TempDir()
	m := NewManager(Config{Shell: shell, Home: dir, API: &fakeAPI{}, GenEpoch: 5})
	c1 := NewClient(80, 24)
	s, err := m.CreateOrAttachDefault(c1)
	if err != nil {
		t.Fatal(err)
	}
	b1 := drainClient(c1)
	// Set a unique prompt so a readline redraw is detectable in the output stream.
	if _, err := s.Write([]byte("PS1='__RLPROMPT__ '\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(b1.String(), "__RLPROMPT__")
	})
	before := strings.Count(b1.String(), "__RLPROMPT__")
	// Attach a second client with the SAME geometry: recomputeWin keeps 80x24, so
	// pty.Setsize is a silent no-op and ONLY the explicit SIGWINCH-on-attach can
	// make readline redraw the idle prompt. The redraw lands on c1 (the existing
	// client receives none of c2's replay/clear frames), so any new "__RLPROMPT__"
	// in c1's stream after the attach is the repaint we're guarding.
	c2 := NewClient(80, 24)
	if _, err := m.Attach(s.ID(), c2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return strings.Count(b1.String(), "__RLPROMPT__") > before
	})
	s.Detach(c1)
	s.Detach(c2)
}

// TestSmallestAttachedResize: the PTY size is the SMALLEST of attached clients,
// recomputed on attach/detach/resize.
func TestSmallestAttachedResize(t *testing.T) {
	m := testManager(t, &fakeAPI{})
	c1 := NewClient(120, 40)
	s, err := m.CreateOrAttachDefault(c1)
	if err != nil {
		t.Fatal(err)
	}
	drainClient(c1)
	// One client: size is its own.
	if s.winW != 120 || s.winH != 40 {
		t.Fatalf("single-client size = %dx%d, want 120x40", s.winW, s.winH)
	}
	// Attach a smaller client → size shrinks to the smallest.
	c2 := NewClient(80, 24)
	if _, err := m.Attach(s.ID(), c2); err != nil {
		t.Fatal(err)
	}
	drainClient(c2)
	if s.winW != 80 || s.winH != 24 {
		t.Fatalf("two-client size = %dx%d, want 80x24 (smallest)", s.winW, s.winH)
	}
	// Detach the small one → size grows back to the remaining client.
	s.Detach(c2)
	if s.winW != 120 || s.winH != 40 {
		t.Fatalf("after detach size = %dx%d, want 120x40", s.winW, s.winH)
	}
	// Last detach HOLDS the size (no change).
	s.Detach(c1)
	if s.winW != 120 || s.winH != 40 {
		t.Fatalf("after last detach size = %dx%d, want held 120x40", s.winW, s.winH)
	}
}

// TestCleanEndAndProcessGroupReap: a setsid child holding the slave open does
// not keep the session alive — cmd.Wait on the root shell drives the end and
// kill(-pgid) reaps the holder.
func TestCleanEndAndProcessGroupReap(t *testing.T) {
	api := &fakeAPI{}
	m := testManager(t, api)
	c := NewClient(80, 24)
	s, err := m.CreateOrAttachDefault(c)
	if err != nil {
		t.Fatal(err)
	}
	drainClient(c)
	// Spawn a setsid child that holds the slave (its own stdin) open, then exit
	// the root shell. cmd.Wait must fire on the shell exit (NOT block on PTY EOF
	// held open by the child), and waitLoop's kill(-pgid) reaps the child.
	if _, err := s.Write([]byte("setsid sleep 300 & exit 0\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("session did not end on root-shell exit (PTY-EOF blocked?)")
	}
	if s.ExitCode() != 0 {
		t.Fatalf("exit code = %d, want 0", s.ExitCode())
	}
	// Removed from the registry, and an end event POSTed.
	if m.HeldLivePTYs() != 0 {
		t.Fatalf("HeldLivePTYs after end = %d, want 0", m.HeldLivePTYs())
	}
	waitFor(t, 2*time.Second, func() bool { return api.endCount() == 1 })
}

// TestExitCodePropagates: the root shell's exit code is reported accurately.
func TestExitCodePropagates(t *testing.T) {
	m := testManager(t, &fakeAPI{})
	c := NewClient(80, 24)
	s, err := m.CreateOrAttachDefault(c)
	if err != nil {
		t.Fatal(err)
	}
	drainClient(c)
	if _, err := s.Write([]byte("exit 7\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("no end")
	}
	if s.ExitCode() != 7 {
		t.Fatalf("exit code = %d, want 7", s.ExitCode())
	}
}

// TestDetachNeverEndsSession: detaching the only client leaves the shell
// running.
func TestDetachNeverEndsSession(t *testing.T) {
	m := testManager(t, &fakeAPI{})
	c := NewClient(80, 24)
	s, err := m.CreateOrAttachDefault(c)
	if err != nil {
		t.Fatal(err)
	}
	drainClient(c)
	s.Detach(c)
	// Session still held; shell still alive.
	if m.HeldLivePTYs() != 1 {
		t.Fatalf("HeldLivePTYs after detach = %d, want 1 (detach must not end)", m.HeldLivePTYs())
	}
	select {
	case <-s.Done():
		t.Fatal("session ended on detach — it must not")
	case <-time.After(200 * time.Millisecond):
	}
	// Reattach proves the shell is still there.
	c2 := NewClient(80, 24)
	if _, err := m.Attach(s.ID(), c2); err != nil {
		t.Fatalf("reattach after detach: %v", err)
	}
	s.Detach(c2)
}

// TestBootReconcileTombstonesAtBumpedEpoch: a fresh process bumps the epoch and
// tombstones at the bumped value.
func TestBootReconcileTombstonesAtBumpedEpoch(t *testing.T) {
	dir := t.TempDir()
	// First boot: no file → E=0 → bump to 1.
	ge1, err := ReadAndBumpGenEpoch(dir)
	if err != nil || ge1 != 1 {
		t.Fatalf("first bump = %d, %v; want 1", ge1, err)
	}
	// Persisted to disk.
	b, _ := os.ReadFile(filepath.Join(dir, genEpochFile))
	if v, _ := strconv.Atoi(strings.TrimSpace(string(b))); v != 1 {
		t.Fatalf("persisted epoch = %q, want 1", string(b))
	}
	// Second boot: reads 1 → bumps to 2.
	ge2, err := ReadAndBumpGenEpoch(dir)
	if err != nil || ge2 != 2 {
		t.Fatalf("second bump = %d, %v; want 2", ge2, err)
	}

	api := &fakeAPI{}
	m := NewManager(Config{Shell: "/bin/sh", Home: dir, API: api, GenEpoch: ge2})
	if err := m.BootReconcile(context.Background()); err != nil {
		t.Fatalf("BootReconcile: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.tombstones) != 1 || api.tombstones[0] != 2 {
		t.Fatalf("tombstones = %v, want [2] (the bumped epoch)", api.tombstones)
	}
}

// TestSyncSnapshotShape: SyncNow posts a snapshot keyed by id carrying name +
// attached count.
func TestSyncSnapshotShape(t *testing.T) {
	api := &fakeAPI{}
	m := testManager(t, api)
	c := NewClient(80, 24)
	s, err := m.CreateOrAttachDefault(c) // fires a sync on attach
	if err != nil {
		t.Fatal(err)
	}
	drainClient(c)
	m.SyncNow()
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.syncs) == 0 {
		t.Fatal("no sync posted")
	}
	last := api.syncs[len(api.syncs)-1]
	meta, ok := last[s.ID()]
	if !ok {
		t.Fatalf("snapshot missing session %s: %v", s.ID(), last)
	}
	if meta.Name != "main" || meta.AttachedCount != 1 {
		t.Fatalf("snapshot meta = %+v, want name=main attached=1", meta)
	}
	s.Detach(c)
}

var _ io.Writer = writerToSessionForTest{}

// writerToSessionForTest is a compile guard mirroring the sshserver adapter,
// kept here so the sessions package's Write contract is exercised.
type writerToSessionForTest struct{ s *Session }

func (w writerToSessionForTest) Write(p []byte) (int, error) { return w.s.Write(p) }
