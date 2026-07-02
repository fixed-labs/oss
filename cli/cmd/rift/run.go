package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/fixed-labs/oss/cli/internal/broker"
	"github.com/fixed-labs/oss/cli/internal/config"
)

// Distinct non-zero exit codes for `devbox run`'s own failures, so an agent (or
// a wrapping script) can branch on the reason. A successful child passes through
// the child's own exit code unchanged. These sit in 119-124 — a range that
// conventional CLI tools and sysexits.h avoid (64-78 is the sysexits band; 125-128
// are git/shell conventions) — to MINIMIZE collision with a code the child itself
// might emit. Collision is minimized, not impossible, so for the broker/connection-down
// cases the stderr message (not the numeric code) is the authoritative signal.
const (
	exitUsage               = 120 // bad flags / flag combination
	exitUnknownSec          = 121 // unknown / unmapped secret
	exitUnreachable         = 122 // broker handler is down (incl. no connection attached)
	exitBrokerErr           = 123 // handler returned an error other than unknown-secret
	exitInternal            = 124 // local failure (materialize write, exec setup)
	exitMultipleConnections = 119 // >1 connection attached — broker can't pick which to dial
)

// runOpts is the parsed `devbox run` invocation.
type runOpts struct {
	secrets       []string // --secret (repeatable)
	shell         bool     // --shell
	materializeTo string   // --materialize-to <path>
	cmd           []string // trailing -- cmd...
}

// cmdRun is the `devbox run` shim.
// It fetches named secrets from the connect-side broker handler over the tunnel, injects them
// into a forked child (never exec — it runs the audit step after the child
// exits), proxies stdio, and passes through the child's exit code. The credential
// never appears in stdout/stderr and is never written to the box (the one audited
// exception is --materialize-to).
//
// It returns nil on a clean run (it calls os.Exit with the child's code itself,
// since main's error path collapses everything to exit 1). On a usage/broker
// error it prints a message and os.Exit's a distinct code.
func cmdRun(ctx context.Context, args []string) error {
	opts, err := parseRunArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rift run: %v\n", err)
		fmt.Fprint(os.Stderr, runUsage)
		os.Exit(exitUsage)
	}

	cfg, err := config.FromEnvOrFile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rift run: %v\n", err)
		os.Exit(exitInternal)
	}

	bc := &broker.Client{} // discovers the connection's overlay IP from the agent-published file, dials BrokerPort over wg0
	ad := newAuditor(cfg)

	switch {
	case opts.materializeTo != "":
		os.Exit(runMaterialize(ctx, bc, ad, opts))
	case opts.shell:
		os.Exit(runShell(ctx, bc, ad, opts))
	default:
		os.Exit(runCommand(ctx, bc, ad, opts))
	}
	return nil // unreachable
}

const runUsage = `usage:
  rift run --secret <name> [--secret <name>...] -- <cmd> [args...]
  rift run --shell --secret <name> [--secret <name>...]
  rift run --secret <name> --materialize-to <path>

  --secret <name>        a registered secret (e.g. aws, npm); repeatable
  --shell                open an interactive subshell with the secrets injected
  --materialize-to <p>   write the secret's raw source to <p> (exactly one --secret;
                         no --shell, no trailing command). The file PERSISTS.
`

// parseRunArgs hand-parses the run flags. The trailing `-- cmd...` and the
// repeatable --secret rule out flag.FlagSet, so this matches the surrounding
// hand-rolled parsing (cf. splitRepoFlag). It enforces every flag-combination
// constraint.
func parseRunArgs(args []string) (runOpts, error) {
	var o runOpts
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			o.cmd = append(o.cmd, args[i+1:]...)
			break
		}
		switch {
		case a == "--secret" || a == "-secret":
			if i+1 >= len(args) {
				return o, errors.New("--secret needs a value")
			}
			o.secrets = append(o.secrets, args[i+1])
			i++
		case strings.HasPrefix(a, "--secret="):
			o.secrets = append(o.secrets, a[len("--secret="):])
		case a == "--shell" || a == "-shell":
			o.shell = true
		case a == "--materialize-to" || a == "-materialize-to":
			if i+1 >= len(args) {
				return o, errors.New("--materialize-to needs a path")
			}
			o.materializeTo = args[i+1]
			i++
		case strings.HasPrefix(a, "--materialize-to="):
			o.materializeTo = a[len("--materialize-to="):]
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("unknown flag %q", a)
		default:
			return o, fmt.Errorf("unexpected argument %q (a command goes after `--`)", a)
		}
	}

	if len(o.secrets) == 0 {
		return o, errors.New("at least one --secret is required")
	}
	for _, s := range o.secrets {
		if strings.TrimSpace(s) == "" {
			return o, errors.New("empty --secret name")
		}
	}

	switch {
	case o.materializeTo != "":
		if o.shell {
			return o, errors.New("--materialize-to is incompatible with --shell")
		}
		if len(o.cmd) > 0 {
			return o, errors.New("--materialize-to takes no trailing command")
		}
		if len(o.secrets) != 1 {
			return o, errors.New("--materialize-to takes exactly one --secret")
		}
	case o.shell:
		if len(o.cmd) > 0 {
			return o, errors.New("--shell takes no trailing command")
		}
	default:
		if len(o.cmd) == 0 {
			return o, errors.New("no command — put it after `--` (or use --shell)")
		}
	}
	return o, nil
}

// fetchInjectEnv fetches and merges the injected env pairs for every --secret.
// On a broker error it prints a clear message and returns the exit code to use;
// ok is false when the caller should os.Exit(code).
func fetchInjectEnv(ctx context.Context, bc *broker.Client, secs []string) (env []string, code int, ok bool) {
	for _, s := range secs {
		pairs, err := bc.FetchInject(ctx, s)
		if err != nil {
			return nil, classifyBrokerErr(err, s), false
		}
		for _, p := range pairs {
			env = append(env, p.Name+"="+p.Value)
		}
	}
	return env, 0, true
}

// classifyBrokerErr prints a message and returns the distinct exit code for a
// box-side broker error. The unreachable case tells the agent the connection
// is down — NOT "go read the file" (that would defeat the broker).
func classifyBrokerErr(err error, secret string) int {
	switch {
	case errors.Is(err, broker.ErrMultipleConnections):
		fmt.Fprintf(os.Stderr,
			"rift run: %v.\nThis box has more than one active connection, so the secret broker can't tell which to use.\n"+
				"If you only have one `rift connect` open, the others are stale connections from earlier sessions that\n"+
				"didn't disconnect cleanly — they clear on their own within 12h, or an operator can clear them now.\n", err)
		return exitMultipleConnections
	case errors.Is(err, broker.ErrUnreachable):
		fmt.Fprintf(os.Stderr,
			"rift run: cannot reach the secret broker — this box has no active connection (your `rift connect` is down).\n"+
				"Reconnect with `rift connect` and retry; do not read the credential file directly.\n")
		return exitUnreachable
	case errors.Is(err, broker.ErrUnknownSecretRemote):
		fmt.Fprintf(os.Stderr, "rift run: %s: %v\n", secret, err)
		return exitUnknownSec
	default:
		fmt.Fprintf(os.Stderr, "rift run: %s: %v\n", secret, err)
		return exitBrokerErr
	}
}

// runCommand is the default path: fetch env for each secret, fork the child with
// the merged injected env, proxy stdio, wait, post audit, and return the child's
// exit code.
func runCommand(ctx context.Context, bc *broker.Client, ad *auditor, o runOpts) int {
	env, code, ok := fetchInjectEnv(ctx, bc, o.secrets)
	if !ok {
		return code
	}
	cmd := exec.CommandContext(ctx, o.cmd[0], o.cmd[1:]...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	exit := runChild(cmd)
	ad.post(ctx, o.secrets, strings.Join(o.cmd, " "), &exit, exit == 0)
	return exit
}

// runShell opens an interactive subshell with the merged injected env for the
// session (no trailing command). The value lives in that shell's environment for
// the session but never on the box's disk.
func runShell(ctx context.Context, bc *broker.Client, ad *auditor, o runOpts) int {
	env, code, ok := fetchInjectEnv(ctx, bc, o.secrets)
	if !ok {
		return code
	}
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/bash"
	}
	cmd := exec.CommandContext(ctx, sh)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	fmt.Fprintf(os.Stderr, "rift run: opening a subshell with %s injected (exit to end)\n", strings.Join(o.secrets, ", "))
	exit := runChild(cmd)
	ad.post(ctx, o.secrets, "--shell session", &exit, exit == 0)
	return exit
}

// runMaterialize writes the secret's raw source bytes to the named path — the one
// deliberate, audited exception to "nothing left on the box." The file persists.
func runMaterialize(ctx context.Context, bc *broker.Client, ad *auditor, o runOpts) int {
	secret := o.secrets[0]
	raw, err := bc.FetchMaterialize(ctx, secret)
	if err != nil {
		return classifyBrokerErr(err, secret)
	}
	if len(raw) == 0 {
		// An empty credential file is a misconfiguration, not a success — refuse
		// it rather than writing a 0-byte file and reporting "materialized".
		fmt.Fprintf(os.Stderr,
			"rift run: %s resolved to an empty credential; refusing to write an empty file to %s\n",
			secret, o.materializeTo)
		ad.post(ctx, o.secrets, "--materialize-to "+o.materializeTo, nil, false)
		return exitBrokerErr
	}
	// 0600 — a credential file, owner-only.
	if err := os.WriteFile(o.materializeTo, raw, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "rift run: write %s: %v\n", o.materializeTo, err)
		ad.post(ctx, o.secrets, "--materialize-to "+o.materializeTo, nil, false)
		return exitInternal
	}
	// Loud notice: this is the audited escape hatch, and the file persists.
	fmt.Fprintf(os.Stderr,
		"rift run: NOTICE — materialized secret %q to %s (0600). This file PERSISTS on the box; "+
			"you named the path and own its lifecycle. Prefer `rift run --secret %s -- <cmd>` where the tool allows it.\n",
		secret, o.materializeTo, secret)
	ad.post(ctx, o.secrets, "--materialize-to "+o.materializeTo, nil, true)
	return 0
}

// runChild runs cmd (stdio already wired) and returns its exit code, mapping a
// failure to start or a signal to a non-zero code. The credential is in cmd.Env
// only; it never appears in stdout/stderr (which are the agent's, not ours).
func runChild(cmd *exec.Cmd) int {
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if code := ee.ExitCode(); code >= 0 {
				return code
			}
			return 128 // killed by a signal — conventional 128+? collapsed to 128
		}
		// Failed to start (e.g. command not found): distinct from a child exit code.
		fmt.Fprintf(os.Stderr, "rift run: %v\n", err)
		return exitInternal
	}
	return 0
}
