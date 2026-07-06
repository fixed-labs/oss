package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/fixed-labs/oss/cli/internal/broker"
	"github.com/fixed-labs/oss/cli/internal/secrets"
	"github.com/fixed-labs/oss/cli/internal/tunnel"
)

const (
	sshExecTimeout = 15 * time.Second // per-attempt bound on one box exec
	sshExecTries   = 3                // retries cover the post-tunnel wg-auth race
)

// sshExecer runs a remote script on the box via the system ssh against the
// bridged, host-key-pinned tunnel (the same dataplane the interactive
// devbox-session subsystem rides). The script is the remote command; secret
// bytes ride stdin (never argv).
// Each exec is time-bounded and retried (all box scripts are idempotent), so a
// stalled handshake can't wedge connect and a transient auth race self-heals.
type sshExecer struct {
	host, port, khFile, user string
}

func (e *sshExecer) Run(ctx context.Context, script string, stdin []byte) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < sshExecTries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		cctx, cancel := context.WithTimeout(ctx, sshExecTimeout)
		out, err := e.runOnce(cctx, script, stdin)
		cancel()
		if err == nil {
			return out, nil
		}
		lastErr = err
		if ctx.Err() != nil { // parent canceled (SIGINT) — stop retrying
			return out, ctx.Err()
		}
	}
	return nil, lastErr
}

func (e *sshExecer) runOnce(ctx context.Context, script string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ssh",
		"-p", e.port,
		"-o", "UserKnownHostsFile="+e.khFile,
		"-o", "StrictHostKeyChecking=yes",
		"-o", "PreferredAuthentications=none",
		"-o", "PubkeyAuthentication=no",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		e.user+"@"+e.host, script)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out := &capWriter{max: maxBoxOutput}
	errb := &capWriter{max: maxBoxOutput}
	cmd.Stdout = out
	cmd.Stderr = errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%v: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

const maxBoxOutput = 1 << 20 // cap box stdout/stderr so a misbehaving box can't OOM the CLI

// capWriter buffers up to max bytes and silently drops the rest (claiming full
// writes so the child neither errors nor blocks).
type capWriter struct {
	buf bytes.Buffer
	n   int
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.n; room > 0 {
		if len(p) <= room {
			w.buf.Write(p)
			w.n += len(p)
		} else {
			w.buf.Write(p[:room])
			w.n = w.max
		}
	}
	return len(p), nil
}

func (w *capWriter) Bytes() []byte  { return w.buf.Bytes() }
func (w *capWriter) String() string { return w.buf.String() }

// reconcileSecrets runs the secret push during connect, between tunnel-up and
// the shell. Best-effort: any failure warns and never blocks the shell. Box I/O
// and source commands are each bounded (in sshExecer / readSource); interactive
// prompts are intentionally unbounded (Ctrl-C still aborts via the parent ctx).
// Returns whether to enable ssh agent forwarding (a std:ssh-mapped key) —
// honored even on a partial-sync error (see below).
func reconcileSecrets(ctx context.Context, host, port, khFile, repoID string) bool {
	if repoID == "" {
		return false
	}
	repoID = secretsRepoID(repoID)
	ex := &sshExecer{host: host, port: port, khFile: khFile, user: loginUser()}
	res, err := secrets.Reconcile(ctx, ex, repoID, secrets.Options{
		In:          os.Stdin,
		Out:         os.Stderr,
		Interactive: isTerminal(os.Stdin),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "rift: secrets sync skipped: %v\n", err)
		// Fall through — do NOT discard res.ForwardAgent. The std:ssh forwarding
		// decision is resolved from the parsed manifest in Reconcile's first pass,
		// BEFORE the per-secret box I/O (hash reads / pushes) that produces most
		// errors, and agent forwarding pushes no bytes to the box. So a transient
		// box-I/O hiccup mid-sync must not silently disable agent forwarding for the
		// whole connection. res.ForwardAgent is the zero value (false) when the
		// failure happened before the manifest was parsed, so honoring it here never
		// enables forwarding we didn't actually resolve.
	}
	return res.ForwardAgent
}

// startBrokerHandler brings up the laptop-side secret-broker handler on the
// tunnel's overlay listener and serves it for the session. It loads the same
// user config
// the connect-time reconcile uses and resolves named secrets against repoID
// (the workspace's canonical repo, reduced to the secrets host/owner/name form
// at this seam). Returns a stop func that closes the listener (also
// closed when ctx is cancelled). Best-effort: a load or listen failure warns and
// returns a no-op stop — the shell still works; `devbox run` on the box then gets
// an unreachable-handler error, which it reports as "the laptop session is down."
func startBrokerHandler(ctx context.Context, t *tunnel.Tunnel, repoID string) (stop func()) {
	noop := func() {}
	if repoID == "" {
		return noop // no repo → no per-repo source mapping to resolve against
	}
	repoID = secretsRepoID(repoID)
	uc, err := secrets.LoadUserConfig("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "rift: secret broker disabled: %v\n", err)
		return noop
	}
	ln, err := t.ListenOverlayTCP(broker.BrokerPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rift: secret broker disabled: %v\n", err)
		return noop
	}
	h := broker.NewHandler(broker.NewStaticProvider(uc, repoID), brokerLogf)
	go func() {
		if err := h.Serve(ctx, ln); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "rift: secret broker stopped: %v\n", err)
		}
	}()
	return func() { _ = ln.Close() }
}

// brokerLogf forwards the handler's connection-level diagnostics to stderr. The
// handler never passes a credential value here (it logs only value-free
// connection faults), so this is safe.
func brokerLogf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "rift: "+format+"\n", args...)
}

// isTerminal is a dependency-free heuristic (a real isatty would need x/term).
// It's true for any char device, so a redirected `connect < /dev/null` is
// (mis)classified interactive — but that degrades safely: reading /dev/null is
// instant EOF, which prompt() treats as "no" → the secret is skipped, exactly as
// a non-interactive run should. A plain pipe is correctly non-char → false.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func cmdSecrets(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: rift secrets <status|map> …")
	}
	switch args[0] {
	case "status":
		return secretsStatus(ctx, args[1:])
	case "map":
		return secretsMap(ctx, args[1:])
	default:
		return fmt.Errorf("unknown subcommand: rift secrets %s (want status|map)", args[0])
	}
}

// secretsStatus previews the resolution against the LOCAL .rift/secrets.json
// (no box needed) — what would be pushed, and which keys still need a source.
func secretsStatus(_ context.Context, args []string) error {
	repoFlag, forgeFlag, pos, err := splitRepoFlag(args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return fmt.Errorf("usage: rift secrets status [--repo REPO] [--forge F]")
	}
	repo, err := resolveRepoArg(repoFlag, forgeFlag)
	if err != nil {
		return err
	}

	manifest, err := secrets.LoadRepoManifestFile(localManifestPath())
	if err != nil {
		return err
	}
	if manifest == nil {
		fmt.Println("No .rift/secrets.json found locally — nothing to preview.")
		return nil
	}
	uc, err := secrets.LoadUserConfig("")
	if err != nil {
		return err
	}
	resolved, unmapped, errs := secrets.Resolve(manifest, uc, repo)

	fmt.Printf("repo: %s   (local preview — the live sync reads the manifest from the box on connect)\n\n", secrets.QualifiedRepoID(repo))
	if len(resolved) > 0 {
		fmt.Printf("%-16s  %-8s  %-6s  %-44s  %s\n", "KEY", "STRATEGY", "TMPFS", "DEST", "SOURCE")
		for _, r := range resolved {
			tmpfs, dest, src := "-", "-", sourceDesc(r.Source)
			switch r.Strategy {
			case secrets.StrategyForward:
				src = "(agent forwarding)"
			case secrets.StrategyInject:
				// Brokered — injected into a `devbox run` child on use, never written
				// to the box. Show the env var name(s); there is no on-box dest.
				dest = "brokered — injected on use, not on the box (" + envVarList(r.EnvNames) + ")"
			case secrets.StrategyEnv:
				dest = envVarList(r.EnvNames) // exported as an env var, not a file the user reads
				tmpfs = "yes"
			default:
				dest = "~/" + r.Dest
				if r.Tmpfs {
					tmpfs = "yes"
				} else {
					tmpfs = "no"
				}
			}
			fmt.Printf("%-16s  %-8s  %-6s  %-44s  %s\n", r.Key, r.Strategy, tmpfs, dest, src)
		}
	}
	if len(unmapped) > 0 {
		fmt.Printf("\nunmapped (no source — will be skipped):\n")
		for _, u := range unmapped {
			line := fmt.Sprintf("  %-16s  %s", u.Key, u.Label())
			if u.Desc != "" {
				line += "  (" + secrets.SanitizeTerminal(u.Desc) + ")"
			}
			fmt.Println(line)
		}
		mapHint := "rift secrets map <key> <source>"
		if repoFlag != "" {
			mapHint += " --repo " + secrets.QualifiedRepoID(repo)
		}
		fmt.Printf("\nmap one with:  %s\n", mapHint)
	}
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}
	return nil
}

// secretsMap writes a key→source mapping into the user config.
func secretsMap(_ context.Context, args []string) error {
	repoFlag, forgeFlag, pos, err := splitRepoFlag(args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return fmt.Errorf("usage: rift secrets map <key> <source> [--repo REPO] [--forge F]\n" +
			"  <source> is a path, the literal `forward`, or `cmd:<command>`")
	}
	key, err := secrets.ParseKey(pos[0])
	if err != nil {
		return err
	}
	// A bare name like `aws` (no explicit namespace) parses as local:aws but
	// almost certainly meant the std: key (the std registry is keyed by bare name)
	// — that would create a mapping no manifest references. Make the user qualify
	// it. An explicit `local:aws` is honored as-is.
	if !strings.Contains(pos[0], ":") && secrets.IsStdName(key.Name) {
		return fmt.Errorf("%q parses as local:%s, which no manifest declares — did you mean std:%s? (bare names default to local:)", pos[0], key.Name, key.Name)
	}
	src, err := parseSourceArg(pos[1])
	if err != nil {
		return err
	}
	repo, err := resolveRepoArg(repoFlag, forgeFlag)
	if err != nil {
		return err
	}
	qualified := secrets.QualifiedRepoID(repo)

	uc, err := secrets.LoadUserConfig("")
	if err != nil {
		return err
	}
	uc.SetMapping(qualified, key.String(), src)
	if err := uc.Save(); err != nil {
		return err
	}
	fmt.Printf("mapped %s → %s for %s\n", key, sourceDesc(src), qualified)
	return nil
}

// splitRepoFlag pulls --repo/-repo and --forge/-forge (space- or =-separated)
// out of args and returns them plus the remaining positionals. Go's flag
// package stops at the first positional, so the documented
// `map <key> <source> --repo R` order would otherwise silently ignore --repo.
func splitRepoFlag(args []string) (repo, forge string, pos []string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--": // end of flags: the rest are positionals (e.g. a -dash source)
			pos = append(pos, args[i+1:]...)
			return repo, forge, pos, nil
		case a == "--repo" || a == "-repo":
			if i+1 >= len(args) {
				return "", "", nil, fmt.Errorf("--repo needs a value")
			}
			repo = args[i+1]
			i++
		case strings.HasPrefix(a, "--repo=") || strings.HasPrefix(a, "-repo="):
			repo = a[strings.IndexByte(a, '=')+1:]
			if repo == "" {
				return "", "", nil, fmt.Errorf("--repo needs a non-empty value")
			}
		case a == "--forge" || a == "-forge":
			if i+1 >= len(args) {
				return "", "", nil, fmt.Errorf("--forge needs a value")
			}
			forge = args[i+1]
			i++
		case strings.HasPrefix(a, "--forge=") || strings.HasPrefix(a, "-forge="):
			forge = a[strings.IndexByte(a, '=')+1:]
			if forge == "" {
				return "", "", nil, fmt.Errorf("--forge needs a non-empty value")
			}
		case strings.HasPrefix(a, "-"):
			return "", "", nil, fmt.Errorf("unknown flag %q (only --repo/--forge; use -- before a source that starts with -)", a)
		default:
			pos = append(pos, a)
		}
	}
	return repo, forge, pos, nil
}

// secretsRepoID reduces a canonical "forge:host/owner/name" repo id to the
// secrets layer's qualified "host/owner/name" form by dropping the "<forge>:"
// prefix — the one-seam conversion at the secrets boundary (the secrets
// config-key grammar itself is unchanged and knows no forge). Strings without
// a forge-enum prefix pass through untouched.
func secretsRepoID(repo string) string {
	if i := strings.IndexByte(repo, ':'); i > 0 && forgeEnum[strings.ToLower(repo[:i])] {
		return repo[i+1:]
	}
	return repo
}

// resolveRepoArg returns the repo id the secrets layer keys on: the inferred
// cwd repo when the flag is empty, else the explicit value — both resolved
// through the same flow-1 + canonicalizer as every other repo ingress, then
// reduced to the secrets-qualified host/owner/name form.
func resolveRepoArg(flag, forgeFlag string) (string, error) {
	canonical, err := resolveRepo(flag, forgeFlag)
	if err != nil {
		return "", err
	}
	return secretsRepoID(canonical), nil
}

func parseSourceArg(s string) (secrets.Source, error) {
	switch {
	case s == "forward":
		return secrets.Source{Path: "forward"}, nil
	case strings.HasPrefix(s, "cmd:"):
		cmd := strings.TrimSpace(strings.TrimPrefix(s, "cmd:"))
		if cmd == "" {
			return secrets.Source{}, fmt.Errorf("empty cmd: source")
		}
		return secrets.Source{Cmd: cmd}, nil
	case s == "":
		return secrets.Source{}, fmt.Errorf("empty source")
	default:
		return secrets.Source{Path: s}, nil
	}
}

// envVarList renders one or more env var names as `$A` / `$A $B $C`.
func envVarList(names []string) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = "$" + n
	}
	return strings.Join(parts, " ")
}

func sourceDesc(s secrets.Source) string {
	switch {
	case s.Cmd != "":
		return "cmd:" + s.Cmd
	case s.Path == "forward":
		return "forward"
	case s.Path != "":
		return s.Path
	default:
		return "(unmapped)"
	}
}

// localManifestPath finds the repo's manifest from the git toplevel, falling
// back to the cwd.
func localManifestPath() string {
	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		if top := strings.TrimSpace(string(out)); top != "" {
			return filepath.Join(top, ".rift", "secrets.json")
		}
	}
	return filepath.Join(".rift", "secrets.json")
}
