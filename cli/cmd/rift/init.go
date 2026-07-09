package main

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"strings"
)

// initPlaybook is the Markdown playbook `rift init` prints to stdout. It is
// addressed to a coding agent (see the box-first model in the design doc) and
// encodes the Phase-1 six-step spine: Audit → Map → Emit → Splice → Validate →
// Report. This is the sole go:embed in oss/cli — plain go:embed suffices for
// both `go build` (src=./cli) and Pants (the file is a source under this
// package's directory), so no BUILD wiring is required.
//
//go:embed playbooks/init.md
var initPlaybook string

// humanHint is the one-line note printed to STDERR (never stdout) before the
// playbook, so a human who runs `rift init` by hand gets a pointer while an
// agent reading only stdout sees the pristine playbook (design Amendment /
// Open Question 1).
const humanHint = "// note: this prints a machine playbook for a coding agent — see the Rift docs if you're a human.\n"

// Exit-code convention for `rift init` (documented here, applied consistently):
// the design wants exit 2 for *usage* errors (unknown subcommand, bad emit
// flags), but main.go maps a returned error to exit 1. So the init usage-error
// paths do NOT return an error — they print the message to stderr and call
// os.Exit(2) directly, matching main.go's os.Exit(2) for bad top-level usage.
// Non-usage success returns nil (exit 0).

// cmdInit dispatches `rift init` and `rift init emit`. It is offline: it builds
// no client and reads no config. Bad usage exits 2 (see the convention above).
func cmdInit(_ context.Context, args []string) error {
	return runInit(args, os.Stdout, os.Stderr)
}

// runInit is cmdInit with injectable writers, for testing. It returns nil on
// success; usage errors are terminal (os.Exit(2)) rather than returned, so the
// exit-2 contract survives main.go's err→exit-1 mapping.
func runInit(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		// Human hint → stderr; playbook → stdout (agents read only stdout).
		fmt.Fprint(stderr, humanHint)
		fmt.Fprint(stdout, initPlaybook)
		return nil
	}
	switch args[0] {
	case "emit":
		return runInitEmit(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "rift: unknown subcommand: rift init %s (want: emit)\n", args[0])
		osExit(2)
		return nil // unreachable in production; osExit is overridable in tests
	}
}

// osExit is a seam so tests can assert the exit-2 usage paths without killing
// the test process. Defaults to os.Exit.
var osExit = os.Exit

// kv is a parsed --option key=value pair (split on the first '=').
type kv struct {
	k string
	v string
}

// cmdInitEmit parses `rift init emit` flags and prints the two Nix pieces to
// stdout. Offline: no filesystem access, no client, no config. Input errors
// (missing/empty --packages token, --option with no '=') are usage errors →
// os.Exit(2).
func runInitEmit(args []string, stdout, stderr io.Writer) error {
	packages, options, err := parseEmitArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "rift: %v\n", err)
		osExit(2)
		return nil
	}
	out, err := renderEmit(packages, options)
	if err != nil {
		fmt.Fprintf(stderr, "rift: %v\n", err)
		osExit(2)
		return nil
	}
	fmt.Fprint(stdout, out)
	return nil
}

// parseEmitArgs extracts --packages (required, single occurrence, comma-list)
// and --option (repeatable, k=v). It validates flag structure only; the token
// emptiness check lives in renderEmit so the pure renderer owns every
// exit-2-worthy rule the design pins.
func parseEmitArgs(args []string) (packages []string, options []kv, err error) {
	seenPackages := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--packages":
			if seenPackages {
				return nil, nil, fmt.Errorf("--packages given more than once")
			}
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("--packages requires a comma-separated value")
			}
			seenPackages = true
			// Split on comma; trimming and empty-token rejection happen in
			// renderEmit. An empty string ("") yields one empty token there.
			packages = strings.Split(args[i+1], ",")
			i++
		case "--option":
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("--option requires a k=v value")
			}
			raw := args[i+1]
			eq := strings.IndexByte(raw, '=')
			if eq < 0 {
				return nil, nil, fmt.Errorf("--option %q must be k=v (no '=' found)", raw)
			}
			// Split on the FIRST '=' only; values may contain '='.
			options = append(options, kv{k: raw[:eq], v: raw[eq+1:]})
			i++
		default:
			return nil, nil, fmt.Errorf("unexpected argument: %s (want --packages LIST [--option k=v ...])", args[i])
		}
	}
	if !seenPackages {
		return nil, nil, fmt.Errorf("--packages is required")
	}
	return packages, options, nil
}

// renderEmit is the pure, testable core: given the split package tokens and the
// parsed options, it produces the exact two-piece stdout text. It is pure
// string substitution (INV-3) — it interprets/validates no Nix; a malformed
// result is caught later by `nix eval`, not here. The one thing it DOES reject
// is an empty package token (`--packages ""`, `a,,b`, a trailing comma), which
// the design marks a usage error.
func renderEmit(packages []string, options []kv) (string, error) {
	pkgAttrs := make([]string, 0, len(packages))
	for _, p := range packages {
		tok := strings.TrimSpace(p)
		if tok == "" {
			return "", fmt.Errorf("empty package token in --packages (no empty/trailing entries allowed)")
		}
		pkgAttrs = append(pkgAttrs, "pkgs."+tok)
	}

	var b strings.Builder
	b.WriteString("# inputs — merge into your flake's inputs:\n")
	b.WriteString("inputs.rift.url = \"github:fixed-labs/oss\";\n")
	b.WriteString("\n")
	b.WriteString("# outputs — merge into your flake's outputs (whose args must bind `self` and `rift`):\n")
	b.WriteString("fixed-labs.rift = rift.lib.mkRift {\n")
	b.WriteString("  inherit self;\n")
	b.WriteString("  extraModules = [\n")
	b.WriteString("    ({ pkgs, ... }: {\n")
	b.WriteString("      environment.systemPackages = [ " + strings.Join(pkgAttrs, " ") + " ];\n")
	for _, o := range options {
		// k and v are copied VERBATIM; v is a raw Nix expression the caller
		// supplies.
		b.WriteString("      rift.devboxes-base." + o.k + " = " + o.v + ";\n")
	}
	b.WriteString("    })\n")
	b.WriteString("  ];\n")
	b.WriteString("};\n")
	return b.String(), nil
}
