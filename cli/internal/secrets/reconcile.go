package secrets

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"time"
)

const maxManifestBytes = 64 << 10 // the byte cap bounds the entry count

// ioBudget caps cumulative box-I/O + source-read time (not wall clock, so human
// prompt time doesn't count). A var so tests can shrink it.
var ioBudget = 90 * time.Second

// Execer runs a remote script on the box (stdin → stdout) over an already-
// established channel. The CLI implements it via the system `ssh` exec against
// the bridged, host-key-pinned tunnel.
type Execer interface {
	Run(ctx context.Context, script string, stdin []byte) (stdout []byte, err error)
}

// Options configures a Reconcile run.
type Options struct {
	UserConfigPath string    // "" → ~/.config/rift/secrets.json
	In             io.Reader // prompt input; nil disables prompting
	Out            io.Writer // human-facing messages; nil → discarded
	Interactive    bool      // stdin is a TTY; false makes `ask` degrade to skip
}

// Result summarizes a Reconcile run.
type Result struct {
	ForwardAgent bool // a std:ssh key resolved → caller should ssh -A
	Pushed       int
	Skipped      int // unchanged
}

// Reconcile fetches the repo manifest from the box, resolves it against the user
// config, and pushes changed secrets per policy. It is best-effort: a non-nil
// error means a fatal transport/setup failure (caller should warn and still open
// the shell); per-secret problems are reported to Out and don't abort the rest.
func Reconcile(ctx context.Context, ex Execer, repoID string, opt Options) (Result, error) {
	out := opt.Out
	if out == nil {
		out = io.Discard
	}
	var res Result

	// Bound total box I/O so a wedged-but-reachable box can't block the shell for
	// N × per-exec timeout — but measure ACTUAL I/O time (ioSpent), not wall clock,
	// so a human taking their time at a prompt doesn't burn the budget that the
	// push after their approval needs. Each call is parent-rooted (SIGINT aborts)
	// and individually bounded (sshExecer 15s×3, readSource 30s).
	var ioSpent time.Duration
	timedRun := func(script string, stdin []byte) ([]byte, error) {
		t0 := time.Now()
		o, e := ex.Run(ctx, script, stdin)
		ioSpent += time.Since(t0)
		return o, e
	}

	uc, err := LoadUserConfig(opt.UserConfigPath)
	if err != nil {
		return res, err
	}
	repoDir, err := repoDirName(repoID)
	if err != nil {
		return res, err
	}

	// 1. Fetch the manifest + tmpfs-store presence from the box.
	cfgOut, err := timedRun(readConfigScript(repoDir), nil)
	if err != nil {
		return res, fmt.Errorf("read box secrets manifest: %w", err)
	}
	storePresent, manifestB64 := parseConfigOut(cfgOut)
	if manifestB64 == "" {
		return res, nil // repo declares no secrets
	}
	manifestBytes, err := base64.StdEncoding.DecodeString(manifestB64)
	if err != nil {
		fmt.Fprintf(out, "rift: ignoring undecodable secrets manifest from box: %v\n", err)
		return res, nil
	}
	if len(manifestBytes) > maxManifestBytes {
		return res, fmt.Errorf("box manifest too large (%d bytes)", len(manifestBytes))
	}
	manifest, err := ParseRepoManifest(manifestBytes)
	if err != nil {
		fmt.Fprintf(out, "rift: ignoring malformed .rift/secrets.json on box: %v\n", err)
		return res, nil
	}

	// (Resolve dedups by key and caps the DISTINCT count — capping the raw list
	// here would let duplicate padding push a real secret past the cap.)
	resolved, unmapped, rerrs := Resolve(manifest, uc, repoID)
	for _, e := range rerrs {
		fmt.Fprintf(out, "rift: secrets manifest issue: %v\n", e)
	}

	// First pass — peel off the strategies that push NO bytes to the box:
	//
	//   - forward: just flip on ssh agent forwarding (ssh) / warn (gpg).
	//   - inject:  BROKERED. Never written to the box (the load-bearing
	//     invariant): no `copy` dest-write,
	//     no `env.d` file. The broker (`devbox run`, another component) reads the
	//     source and injects EnvNames into a child process at use time, so the push
	//     loop must skip these entirely.
	//
	// Everything else is a copy/env item the push loop acts on.
	var copyItems []Resolved
	for _, r := range resolved {
		switch r.Strategy {
		case StrategyForward:
			switch r.Key.Name {
			case "ssh":
				res.ForwardAgent = true
			case "gpg":
				fmt.Fprintf(out, "rift: std:gpg agent forwarding is not yet supported — skipping\n")
			default:
				fmt.Fprintf(out, "rift: %s is mapped to `forward`, but agent forwarding only applies to ssh/gpg — skipping\n", r.Key)
			}
			continue
		case StrategyInject:
			// Brokered — injected on use by `devbox run`, never pushed to the box.
			continue
		}
		copyItems = append(copyItems, r)
	}

	// Policy first: `off` short-circuits before reading any source, so a repo a
	// user has disabled never triggers a cmd: source (1Password/fnox prompt).
	policy := resolvePolicy(uc, repoID)
	if policy == "off" {
		reportUnmapped(out, unmapped)
		return res, nil
	}

	// Pre-filter to items we might act on, so a source command runs only when it
	// could matter: a non-interactive first-sight target can't be approved, so
	// skip it WITHOUT reading the source.
	var actionable []Resolved
	for _, r := range copyItems {
		if !uc.trustedHas(repoID, trustFingerprint(r)) && (!opt.Interactive || opt.In == nil) {
			fmt.Fprintf(out, "rift: %s → %s needs first-time approval — connect interactively once to approve (skipping)\n", r.Key, destLabel(r))
			continue
		}
		actionable = append(actionable, r)
	}

	// 2. Read current dest hashes + storage location from the box (one round trip).
	hashes := map[string]hashInfo{}
	if len(actionable) > 0 {
		var sb strings.Builder
		for _, r := range actionable {
			sb.WriteString(r.Dest)
			sb.WriteByte('\n')
		}
		hOut, herr := timedRun(readHashesScript(), []byte(sb.String()))
		if herr != nil {
			return res, fmt.Errorf("read box secret state: %w", herr)
		}
		hashes = parseHashes(hOut)
	}

	// 3. For each: read the source, detect drift (content OR storage location —
	// a persistent secret that should now be a tmpfs store symlink post image-
	// upgrade), and on drift apply policy/TOFU and push. The drift check gates
	// the prompt, so an unchanged secret neither prompts nor re-pushes.
	warnedFallback := false
	for _, r := range actionable {
		if ctx.Err() != nil {
			break // interrupted (Ctrl-C): stop prompting/pushing the rest
		}
		if ioSpent >= ioBudget {
			fmt.Fprintf(out, "rift: secrets sync time budget reached — opening shell; remaining secrets not synced this connect\n")
			break
		}
		t0 := time.Now()
		data, derr := readSource(ctx, r.Source)
		ioSpent += time.Since(t0)
		if derr != nil {
			fmt.Fprintf(out, "rift: %s: %v — skipping\n", r.Key, derr)
			continue
		}
		tmpfs := r.Tmpfs && storePresent
		if r.Tmpfs && !storePresent && !warnedFallback {
			fmt.Fprintf(out, "rift: tmpfs secrets store not present on this box (image predates secrets support) — writing to the persistent volume instead\n")
			warnedFallback = true
		}
		st := hashes[r.Dest]
		if st.hash != "-" && st.hash != "" && st.hash == sha256hex(data) && st.onStore == tmpfs {
			res.Skipped++
			continue
		}
		approved, record := decide(ctx, out, opt.In, opt.Interactive, policy, uc, repoID, r, tmpfs)
		if !approved {
			fmt.Fprintf(out, "rift: %s → %s (skipped)\n", r.Key, destLabel(r))
			continue
		}
		if record {
			uc.addTrusted(repoID, trustFingerprint(r))
			if serr := uc.Save(); serr != nil {
				fmt.Fprintf(out, "rift: warning: could not persist trust decision: %v\n", serr)
			}
		}
		if _, perr := timedRun(pushScript(r.Dest, r.Mode, tmpfs, storeName(r.Key)), data); perr != nil {
			fmt.Fprintf(out, "rift: %s: push failed: %v\n", r.Key, perr)
			continue
		}
		res.Pushed++
		where := destLabel(r)
		if tmpfs {
			where += " (tmpfs)"
		}
		fmt.Fprintf(out, "rift: pushed %s → %s\n", r.Key, where)
	}

	reportUnmapped(out, unmapped)
	return res, nil
}

// trustFingerprint binds a TOFU approval to the full (key, dest, mode, tmpfs)
// tuple, so a later manifest change to mode (e.g. 0600→world-readable) or to
// which key fills a dest re-prompts instead of riding the old approval.
func trustFingerprint(r Resolved) string {
	t := "p"
	if r.Tmpfs {
		t = "t"
	}
	return r.Key.String() + "|~/" + r.Dest + "|" + r.Mode + "|" + t
}

// destLabel is the human-facing target for prompts/messages: an env-var/inject
// secret reads as `$NAME`; every other secret as its `~/`-relative path.
func destLabel(r Resolved) string {
	if len(r.EnvNames) > 0 {
		return envLabel(r.EnvNames)
	}
	return "~/" + r.Dest
}

// decide applies policy + TOFU and returns (approved, recordTrust). TOFU-gated:
// a target not yet in `trusted` always prompts (even under auto-push); approval
// is recorded as trusted under auto-push, or under ask only when the user picks
// "always". A non-interactive first sight is skipped (never silently pushed).
func decide(ctx context.Context, out io.Writer, in io.Reader, interactive bool, policy string, uc *UserConfig, repoID string, r Resolved, effectiveTmpfs bool) (approved, record bool) {
	if policy == "off" {
		return false, false
	}
	if uc.trustedHas(repoID, trustFingerprint(r)) {
		return true, false
	}
	if !interactive || in == nil {
		fmt.Fprintf(out, "rift: %s → %s needs first-time approval — connect interactively once to approve (skipping)\n", r.Key, destLabel(r))
		return false, false
	}
	yes, always := prompt(ctx, out, in, r, effectiveTmpfs)
	if !yes {
		return false, false
	}
	return true, always || policy == "auto-push"
}

func prompt(ctx context.Context, out io.Writer, in io.Reader, r Resolved, effectiveTmpfs bool) (yes, always bool) {
	storage := "persistent"
	if effectiveTmpfs {
		storage = "tmpfs"
	}
	fmt.Fprintf(out, "Push %s → %s  (mode %s, %s)?  [y]es / [a]lways / [N]o: ", r.Key, destLabel(r), r.Mode, storage)
	line, err := readLine(ctx, in)
	if err != nil && line == "" {
		return false, false // EOF or ctx-cancel (Ctrl-C) → treat as no
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, false
	case "a", "always":
		return true, true
	default:
		return false, false
	}
}

// readLine reads one line from r WITHOUT read-ahead (one byte at a time), so the
// shared os.Stdin is left exactly after the newline — the interactive ssh shell
// that runs next must not lose buffered keystrokes to a bufio reader. The read
// runs in a goroutine so ctx cancellation (Ctrl-C, which main turns into a ctx
// cancel) returns instead of blocking forever on stdin; the orphaned goroutine
// is harmless (the process is tearing down the connect).
func readLine(ctx context.Context, r io.Reader) (string, error) {
	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var b []byte
		buf := make([]byte, 1)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if buf[0] == '\n' {
					ch <- result{string(b), nil}
					return
				}
				b = append(b, buf[0])
			}
			if err != nil {
				ch <- result{string(b), err}
				return
			}
			if n == 0 {
				runtime.Gosched() // degenerate (0,nil) reader: yield, don't busy-spin
			}
		}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case out := <-ch:
		return out.s, out.err
	}
}

func resolvePolicy(uc *UserConfig, repoID string) string {
	if k, ok := matchRepo(sortedKeys(uc.Repos), repoID); ok {
		if p := uc.Repos[k].Policy; p != "" {
			return p
		}
	}
	return "ask"
}

func parseConfigOut(b []byte) (storePresent bool, manifestB64 string) {
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "STORE 1":
			storePresent = true
		case strings.HasPrefix(line, "CONFIG "):
			manifestB64 = strings.TrimSpace(strings.TrimPrefix(line, "CONFIG "))
		}
	}
	return storePresent, manifestB64
}

// hashInfo is the box's view of a dest: content hash ("-" = absent/dangling)
// and whether it is currently a symlink into the tmpfs store.
type hashInfo struct {
	hash    string
	onStore bool
}

func parseHashes(b []byte) map[string]hashInfo {
	m := map[string]hashInfo{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		hi := hashInfo{hash: parts[1]}
		if len(parts) >= 3 && parts[2] == "1" {
			hi.onStore = true
		}
		m[parts[0]] = hi
	}
	return m
}

func reportUnmapped(out io.Writer, unmapped []Unmapped) {
	if len(unmapped) == 0 {
		return
	}
	sort.Slice(unmapped, func(i, j int) bool { return unmapped[i].Key.String() < unmapped[j].Key.String() })
	fmt.Fprintf(out, "rift: %d declared secret(s) have no source mapping:\n", len(unmapped))
	for _, u := range unmapped {
		line := "  " + u.Key.String() + " → " + u.Label()
		if u.Desc != "" {
			line += "  (" + SanitizeTerminal(u.Desc) + ")"
		}
		fmt.Fprintln(out, line)
	}
	fmt.Fprintln(out, "  map one with:  rift secrets map <key> <source>")
}

// SanitizeTerminal strips control bytes (incl. ANSI escapes) from a manifest-
// derived string before it's printed near a prompt, so a hostile Description
// can't spoof terminal output. Tabs become spaces; other C0/DEL become '?'.
func SanitizeTerminal(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return '?'
		default:
			return r
		}
	}, s)
}
