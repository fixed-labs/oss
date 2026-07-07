package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMain doubles as the self-re-exec harness for the version-command subtests.
//
// When a child process sets RIFT_TEST_RUN_MAIN=1, we do NOT run the test suite:
// we (optionally) inject the package-level `version` var (legal — this test is
// in `package main`), then call the real main() and return. Returning — rather
// than falling through to m.Run() or calling os.Exit after main() — is what
// keeps the child from recursively re-running the whole suite. Every normal
// (non-re-exec) invocation just runs the suite as usual.
func TestMain(m *testing.M) {
	if os.Getenv("RIFT_TEST_RUN_MAIN") == "1" {
		if v := os.Getenv("RIFT_TEST_VERSION"); v != "" {
			version = v
		}
		main()
		return
	}
	os.Exit(m.Run())
}

// TestVersionCommand drives the real `rift version` / `rift --version` dispatch
// as a subprocess (re-execing this already-compiled test binary via os.Args[0]
// under the RIFT_TEST_RUN_MAIN harness above) — no `go build` and no Go
// toolchain at runtime, so it runs green inside the Pants sandbox.
//
// The child is passed ONLY the single command arg (no inherited -test.* flags),
// so the arg reaches os.Args[1] intact. RIFT_LOG_FILE points diag.Setup() at a
// per-test temp path (hermeticity — never the shared state dir), and stdout is
// captured in its own buffer so incidental diag/error output on stderr can't
// pollute the assertion.
func TestVersionCommand(t *testing.T) {
	cases := []struct {
		name       string
		arg        string
		envVersion string
		wantStdout string
	}{
		{name: "--version default", arg: "--version", envVersion: "", wantStdout: "dev\n"},
		{name: "version injected", arg: "version", envVersion: "1.2.3", wantStdout: "1.2.3\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], tc.arg)
			env := []string{
				"RIFT_TEST_RUN_MAIN=1",
				"RIFT_LOG_FILE=" + filepath.Join(t.TempDir(), "rift.log"),
			}
			// Only set RIFT_TEST_VERSION when non-empty so the default row runs
			// with it genuinely unset and observes the real `dev` default.
			if tc.envVersion != "" {
				env = append(env, "RIFT_TEST_VERSION="+tc.envVersion)
			}
			cmd.Env = env

			var out bytes.Buffer
			cmd.Stdout = &out

			if err := cmd.Run(); err != nil {
				t.Fatalf("rift %s: expected exit 0, got %v", tc.arg, err)
			}
			if got := out.String(); got != tc.wantStdout {
				t.Fatalf("rift %s: stdout = %q, want %q", tc.arg, got, tc.wantStdout)
			}
		})
	}
}
