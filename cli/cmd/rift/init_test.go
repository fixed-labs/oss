package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderEmit drives the pure render function directly (no subprocess) for
// representative inputs: single/multiple/dotted packages, one/many/value-with-'='
// options, and none. It asserts the exact substituted text so the emit contract
// (the two comment-delimited pieces) is locked.
func TestRenderEmit(t *testing.T) {
	const header = "# inputs — merge into your flake's inputs:\n" +
		"inputs.rift.url = \"github:fixed-labs/oss\";\n" +
		"\n" +
		"# outputs — merge into your flake's outputs (whose args must bind `self` and `rift`):\n" +
		"fixed-labs.rift = rift.lib.mkRift {\n" +
		"  inherit self;\n" +
		"  extraModules = [\n" +
		"    ({ pkgs, ... }: {\n"
	const footer = "    })\n" +
		"  ];\n" +
		"};\n"

	cases := []struct {
		name     string
		packages []string
		options  []kv
		wantBody string // the lines between header and footer
	}{
		{
			name:     "single package",
			packages: []string{"go"},
			wantBody: "      environment.systemPackages = [ pkgs.go ];\n",
		},
		{
			name:     "multiple packages",
			packages: []string{"go", "ripgrep", "jq"},
			wantBody: "      environment.systemPackages = [ pkgs.go pkgs.ripgrep pkgs.jq ];\n",
		},
		{
			name:     "dotted token",
			packages: []string{"python3Packages.numpy"},
			wantBody: "      environment.systemPackages = [ pkgs.python3Packages.numpy ];\n",
		},
		{
			name:     "tokens are trimmed",
			packages: []string{" go ", "  ripgrep"},
			wantBody: "      environment.systemPackages = [ pkgs.go pkgs.ripgrep ];\n",
		},
		{
			name:     "one option",
			packages: []string{"go"},
			options:  []kv{{k: "loginUser", v: `"dev"`}},
			wantBody: "      environment.systemPackages = [ pkgs.go ];\n" +
				"      rift.devboxes-base.loginUser = \"dev\";\n",
		},
		{
			name:     "multiple options",
			packages: []string{"go"},
			options:  []kv{{k: "loginUser", v: `"dev"`}, {k: "extraGroups", v: `[ "docker" ]`}},
			wantBody: "      environment.systemPackages = [ pkgs.go ];\n" +
				"      rift.devboxes-base.loginUser = \"dev\";\n" +
				"      rift.devboxes-base.extraGroups = [ \"docker\" ];\n",
		},
		{
			name:     "value containing '='",
			packages: []string{"go"},
			options:  []kv{{k: "settings", v: "{ a = 1; }"}},
			wantBody: "      environment.systemPackages = [ pkgs.go ];\n" +
				"      rift.devboxes-base.settings = { a = 1; };\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := renderEmit(tc.packages, tc.options)
			if err != nil {
				t.Fatalf("renderEmit: unexpected error: %v", err)
			}
			want := header + tc.wantBody + footer
			if got != want {
				t.Fatalf("renderEmit mismatch:\n got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// TestRenderEmitEmptyToken asserts an empty package token (from `--packages ""`,
// `a,,b`, or a trailing comma) is a render-level error → the caller exits 2.
func TestRenderEmitEmptyToken(t *testing.T) {
	cases := []struct {
		name     string
		packages []string
	}{
		{name: "empty string", packages: []string{""}},
		{name: "double comma", packages: []string{"a", "", "b"}},
		{name: "trailing comma", packages: []string{"a", ""}},
		{name: "whitespace only", packages: []string{"a", "   "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := renderEmit(tc.packages, nil); err == nil {
				t.Fatalf("renderEmit(%q): expected error for empty token, got nil", tc.packages)
			}
		})
	}
}

// TestParseEmitArgs covers flag parsing: required --packages, first-'=' split for
// --option, and the usage errors (missing --packages, --option with no '=').
func TestParseEmitArgs(t *testing.T) {
	t.Run("splits packages and options", func(t *testing.T) {
		pkgs, opts, err := parseEmitArgs([]string{"--packages", "go,ripgrep", "--option", "loginUser=\"dev\""})
		if err != nil {
			t.Fatalf("parseEmitArgs: %v", err)
		}
		if len(pkgs) != 2 || pkgs[0] != "go" || pkgs[1] != "ripgrep" {
			t.Fatalf("packages = %#v", pkgs)
		}
		if len(opts) != 1 || opts[0].k != "loginUser" || opts[0].v != `"dev"` {
			t.Fatalf("options = %#v", opts)
		}
	})
	t.Run("option splits on first '='", func(t *testing.T) {
		_, opts, err := parseEmitArgs([]string{"--packages", "go", "--option", "settings={ a = 1; }"})
		if err != nil {
			t.Fatalf("parseEmitArgs: %v", err)
		}
		if len(opts) != 1 || opts[0].k != "settings" || opts[0].v != "{ a = 1; }" {
			t.Fatalf("options = %#v", opts)
		}
	})
	t.Run("missing --packages is an error", func(t *testing.T) {
		if _, _, err := parseEmitArgs([]string{"--option", "loginUser=\"dev\""}); err == nil {
			t.Fatal("expected error when --packages absent")
		}
	})
	t.Run("--option without '=' is an error", func(t *testing.T) {
		if _, _, err := parseEmitArgs([]string{"--packages", "go", "--option", "loginUser"}); err == nil {
			t.Fatal("expected error for --option with no '='")
		}
	})
	t.Run("--packages given twice is an error", func(t *testing.T) {
		if _, _, err := parseEmitArgs([]string{"--packages", "go", "--packages", "jq"}); err == nil {
			t.Fatal("expected error for repeated --packages")
		}
	})
	// C3: the malformed-invocation branches of parseEmitArgs — the `default:`
	// "unexpected argument" branch (a stray positional / unknown flag) and the
	// trailing --packages / --option branches (flag present with no following
	// value). We assert only the error outcome, not the message prose.
	t.Run("unexpected positional arg is an error", func(t *testing.T) {
		if _, _, err := parseEmitArgs([]string{"--packages", "go", "extra"}); err == nil {
			t.Fatal("expected error for unexpected trailing argument")
		}
	})
	t.Run("unknown flag is an error", func(t *testing.T) {
		if _, _, err := parseEmitArgs([]string{"--bogus"}); err == nil {
			t.Fatal("expected error for unknown flag (default branch)")
		}
	})
	t.Run("trailing --packages with no value is an error", func(t *testing.T) {
		if _, _, err := parseEmitArgs([]string{"--packages"}); err == nil {
			t.Fatal("expected error for --packages with no following value")
		}
	})
	t.Run("trailing --option with no value is an error", func(t *testing.T) {
		if _, _, err := parseEmitArgs([]string{"--packages", "go", "--option"}); err == nil {
			t.Fatal("expected error for --option with no following value")
		}
	})
}

// TestRunInitEmitExitsTwoOnUsageError verifies the emit usage-error paths call
// osExit(2) (via the overridable seam) rather than returning an error, honoring
// the design's exit-2 requirement despite main.go's err→exit-1 mapping.
func TestRunInitEmitExitsTwoOnUsageError(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "empty token", args: []string{"--packages", "a,,b"}},
		{name: "missing packages", args: []string{"--option", "k=v"}},
		{name: "option no '='", args: []string{"--packages", "go", "--option", "loginUser"}},
		// C3: malformed-invocation branches also exit 2 through the seam.
		{name: "unexpected arg", args: []string{"--packages", "go", "extra"}},
		{name: "trailing --packages", args: []string{"--packages"}},
		{name: "trailing --option", args: []string{"--packages", "go", "--option"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code := captureExit(t, func() {
				var out, errb bytes.Buffer
				_ = runInitEmit(tc.args, &out, &errb)
			})
			if code != 2 {
				t.Fatalf("expected exit 2, got %d", code)
			}
		})
	}
}

// TestRunInitUnknownSubcommandExitsTwo verifies `rift init bogus` exits 2.
func TestRunInitUnknownSubcommandExitsTwo(t *testing.T) {
	code := captureExit(t, func() {
		var out, errb bytes.Buffer
		_ = runInit([]string{"bogus"}, &out, &errb)
	})
	if code != 2 {
		t.Fatalf("expected exit 2 for unknown subcommand, got %d", code)
	}
}

// TestRunInitNoArgsWritesPlaybookAndHint asserts (via injected writers) that
// `rift init` with no args writes the playbook to stdout and the human hint to
// stderr — and that the hint never contaminates stdout.
func TestRunInitNoArgsWritesPlaybookAndHint(t *testing.T) {
	var out, errb bytes.Buffer
	if err := runInit(nil, &out, &errb); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	if out.String() != initPlaybook {
		t.Fatalf("stdout must be exactly the embedded playbook")
	}
	if !strings.Contains(out.String(), "# Rift environment scaffolding playbook") {
		t.Fatal("playbook heading missing from stdout")
	}
	if errb.Len() == 0 {
		t.Fatal("human hint must be written to stderr")
	}
	if strings.Contains(out.String(), "note: this prints a machine playbook") {
		t.Fatal("human hint must NOT appear on stdout")
	}
}

// TestInitCommandNoArgsSubprocess drives the real `rift init` dispatch through
// main() as a subprocess (the RIFT_TEST_RUN_MAIN harness in version_test.go),
// asserting the playbook lands on stdout and the hint on stderr, exit 0.
func TestInitCommandNoArgsSubprocess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "init")
	cmd.Env = []string{
		"RIFT_TEST_RUN_MAIN=1",
		"RIFT_LOG_FILE=" + filepath.Join(t.TempDir(), "rift.log"),
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("rift init: expected exit 0, got %v (stderr: %s)", err, errb.String())
	}
	if !strings.Contains(out.String(), "# Rift environment scaffolding playbook") {
		t.Fatalf("stdout must contain the playbook, got:\n%s", out.String())
	}
	if !strings.Contains(errb.String(), "machine playbook") {
		t.Fatalf("stderr must contain the human hint, got:\n%s", errb.String())
	}
}

// TestInitEmitCommandSubprocess drives the REAL `rift init emit ...` dispatch
// through main() as a subprocess (the RIFT_TEST_RUN_MAIN harness in
// version_test.go), exercising the full main.go → cmdInit → runInit → "emit" →
// runInitEmit wiring AND the real os.Exit(2) path (which the direct-call tests
// bypass via the osExit seam). It asserts the actual process exit code and, for
// the success row, the exact two-piece emit output on stdout.
func TestInitEmitCommandSubprocess(t *testing.T) {
	// The exact stdout renderEmit produces for --packages go,ripgrep with the
	// single loginUser option — the inputs line plus the fixed-labs.rift outputs
	// block. Kept byte-identical to renderEmit so this test also pins the wiring
	// end-to-end.
	const wantEmit = "# inputs — merge into your flake's inputs:\n" +
		"inputs.rift.url = \"github:fixed-labs/oss\";\n" +
		"\n" +
		"# outputs — merge into your flake's outputs (whose args must bind `self` and `rift`):\n" +
		"fixed-labs.rift = rift.lib.mkRift {\n" +
		"  inherit self;\n" +
		"  extraModules = [\n" +
		"    ({ pkgs, ... }: {\n" +
		"      environment.systemPackages = [ pkgs.go pkgs.ripgrep ];\n" +
		"      rift.devboxes-base.loginUser = \"dev\";\n" +
		"    })\n" +
		"  ];\n" +
		"};\n"

	cases := []struct {
		name       string
		args       []string
		wantExit   int
		wantStdout string // only checked when wantExit == 0
	}{
		{
			name:       "success emits both pieces",
			args:       []string{"init", "emit", "--packages", "go,ripgrep", "--option", `loginUser="dev"`},
			wantExit:   0,
			wantStdout: wantEmit,
		},
		{
			name:     "empty package token exits 2",
			args:     []string{"init", "emit", "--packages", "a,,b"},
			wantExit: 2,
		},
		{
			name:     "option with no '=' exits 2",
			args:     []string{"init", "emit", "--option", "novalue"},
			wantExit: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], tc.args...)
			cmd.Env = []string{
				"RIFT_TEST_RUN_MAIN=1",
				"RIFT_LOG_FILE=" + filepath.Join(t.TempDir(), "rift.log"),
			}
			var out, errb bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &errb

			err := cmd.Run()
			gotExit := exitCode(t, err)
			if gotExit != tc.wantExit {
				t.Fatalf("rift %v: exit = %d, want %d (stderr: %s)",
					tc.args, gotExit, tc.wantExit, errb.String())
			}
			if tc.wantExit == 0 && out.String() != tc.wantStdout {
				t.Fatalf("rift %v: stdout mismatch:\n got:\n%s\nwant:\n%s",
					tc.args, out.String(), tc.wantStdout)
			}
		})
	}
}

// exitCode extracts the process exit code from an *exec.Cmd Run() error: nil
// means 0; an *exec.ExitError carries the real code; anything else is a
// launch/harness failure the test can't interpret.
func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	t.Fatalf("subprocess failed to run: %v", err)
	return -1
}

// TestPlaybookContainsLoadBearingCommands guards against shipping an empty or
// structurally-wrong playbook asset. It asserts ONLY the deterministic commands
// the playbook MUST instruct the agent to run — `rift init emit` (the Emit step)
// and the `nix eval … .#fixed-labs.rift.image.drvPath` validation (the Validate
// step). Prose/wording and the step headers are explicitly a DEFERRED, refinable
// item, so this test deliberately does NOT assert on them (that would be brittle);
// only these load-bearing, stable commands are pinned. The bytes checked are the
// same ones `rift init` prints to stdout (initPlaybook), as the other playbook
// tests use.
func TestPlaybookContainsLoadBearingCommands(t *testing.T) {
	for _, want := range []string{
		"rift init emit",                  // step 3 (Emit) — the deterministic emit command
		"nix eval",                        // step 5 (Validate) — the eval command
		".#fixed-labs.rift.image.drvPath", // the attrpath the Validate eval targets
	} {
		if !strings.Contains(initPlaybook, want) {
			t.Errorf("embedded playbook must contain the load-bearing command %q, but it does not", want)
		}
	}
}

// captureExit runs fn with the osExit seam swapped for a recorder, returning the
// code fn passed to osExit (or -1 if it never exited). It panics-and-recovers to
// stop fn at the os.Exit call site, matching how the real os.Exit would abort.
func captureExit(t *testing.T, fn func()) int {
	t.Helper()
	old := osExit
	code := -1
	osExit = func(c int) {
		code = c
		panic(exitSentinel{})
	}
	t.Cleanup(func() { osExit = old })
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(exitSentinel); !ok {
					panic(r)
				}
			}
		}()
		fn()
	}()
	return code
}

type exitSentinel struct{}
