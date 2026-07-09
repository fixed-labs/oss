package main

// init_eval_test.go — the design's "deterministic spine" test for `rift init`
// (plan §"Part B — Testing — the playbook is a shipped prompt").
//
// It proves the EMIT → SPLICE → VALIDATE spine end to end against a real `nix`
// and real nixpkgs: for each fixture it produces a `flake.nix` and asserts
// `nix eval .#fixed-labs.rift.image.drvPath` succeeds. `.drvPath` is
// EVAL-ONLY — it forces the derivation to WHNF but never realises/builds it, so
// this is a one-time nixpkgs fetch (cached thereafter), not a heavy image build.
//
// Three fixtures (testdata/init-fixtures/), matching the plan's (a)/(b)/(c):
//   - node/          — (a) a Node repo; emit's two pieces spliced into the
//                      from-scratch skeleton, then evaled.
//   - python-syslib/ — (b) a Python repo needing a SYSTEM library (zlib);
//                      same splice-into-skeleton path, plus one --option.
//   - existing-flake/— (c) a repo that already has a flake.nix; the test evals
//                      the checked-in expected-merged/flake.nix (the hand-merged
//                      result). It does NOT re-run the merge — merging an
//                      arbitrary flake is the model's runtime job, out of CI
//                      scope. `emit` is not exercised for (c).
//
// (a) and (b) call the SAME renderEmit() the CLI uses, so a drift between the
// emitted contract and what nix accepts fails here.
//
// GATING — this test must never FAIL a hermetic sandbox that lacks `nix`:
//   - If `nix` is not on PATH, or the local oss flake can't be located, the
//     test (and each subtest) t.Skip()s with a clear reason.
//   - When `nix` AND the local oss ARE available (dev machines, the nix-enabled
//     CI container), the eval runs for real and MUST pass.
//
// CRITICAL — the eval overrides the `rift` input to the LOCAL oss via
// `--override-input rift path:<OSS_DIR>`. The published `github:fixed-labs/oss`
// does NOT yet expose `lib.mkRift` (this PR adds it), so the fixtures'
// `inputs.rift.url = "github:fixed-labs/oss"` would fail against the real remote
// — the override redirects it to the in-tree oss.
//
// This is the DETERMINISTIC spine only. Whether the model's *audit* picks the
// right packages for a repo is a separate live-model eval (nightly/manual), not
// unit CI.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// spliceSkeleton splits renderEmit's output into its two comment-delimited
// pieces (inputs / outputs) and drops them into the from-scratch skeleton,
// exactly as a model would at the Splice step. renderEmit emits:
//
//	# inputs — ...:
//	inputs.rift.url = "...";
//	<blank>
//	# outputs — ...:
//	fixed-labs.rift = rift.lib.mkRift { ... };
//
// The blank line between the two comment-delimited blocks is the split point.
func spliceSkeleton(t *testing.T, emit string) string {
	t.Helper()
	const sep = "\n\n"
	i := strings.Index(emit, sep)
	if i < 0 {
		t.Fatalf("emit output has no blank-line separator between its two pieces:\n%s", emit)
	}
	inputsPiece := strings.TrimRight(emit[:i], "\n")
	outputsPiece := strings.TrimRight(emit[i+len(sep):], "\n")
	// Indent each piece to sit under its slot (2 spaces for inputs under the
	// top-level attrset, 4 for outputs under the outputs body). Indentation is
	// cosmetic to Nix, but keeps a dumped fixture legible on failure.
	return strings.NewReplacer(
		"%INPUTS%", indent(inputsPiece, "  "),
		"%OUTPUTS%", indent(outputsPiece, "    "),
	).Replace(`{
%INPUTS%
  outputs = { self, rift, ... }: {
%OUTPUTS%
  };
}
`)
}

// lastLine returns the last non-empty, whitespace-trimmed line of s. `nix eval`
// may write a lock-file `warning:` block before the value on combined output;
// the value we assert on is the final line.
func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if ln == "" {
			continue
		}
		lines[i] = pad + ln
	}
	return strings.Join(lines, "\n")
}

// ossFlakeDir locates the in-tree oss flake directory (the one holding
// oss/flake.nix), in order: (1) $RIFT_OSS_FLAKE; (2) walk up from this test
// source file's dir looking for a dir that contains flake.nix and is named
// "oss"; (3) `git rev-parse --show-toplevel` + "/oss". Returns "" if none
// resolves to an existing oss/flake.nix.
func ossFlakeDir(t *testing.T) string {
	t.Helper()
	isOSS := func(dir string) bool {
		if dir == "" {
			return false
		}
		fi, err := os.Stat(filepath.Join(dir, "flake.nix"))
		return err == nil && !fi.IsDir()
	}

	// (1) explicit env override.
	if env := os.Getenv("RIFT_OSS_FLAKE"); env != "" {
		if isOSS(env) {
			return env
		}
		t.Logf("RIFT_OSS_FLAKE=%q set but has no flake.nix; falling through", env)
	}

	// (2) walk up from this source file's directory. The test lives at
	// oss/cli/cmd/rift/, so the oss dir is three levels up; walk generally.
	if _, file, _, ok := runtime.Caller(0); ok {
		dir := filepath.Dir(file)
		for {
			cand := filepath.Join(dir, "oss")
			if isOSS(cand) {
				return cand
			}
			// Also handle running from within the oss subtree: an ancestor
			// named "oss" that itself holds flake.nix.
			if filepath.Base(dir) == "oss" && isOSS(dir) {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}

	// (3) git toplevel + /oss.
	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		top := strings.TrimSpace(string(out))
		if cand := filepath.Join(top, "oss"); isOSS(cand) {
			return cand
		}
	}
	return ""
}

// ossNixpkgsRev reads the nixpkgs rev the oss flake pins, from oss/flake.lock.
// Used to override the existing-flake fixture's independent `nixpkgs` input to
// the exact rev oss already uses, so the (c) eval needs no extra fetch. Returns
// "" if the lock can't be read/parsed (the caller then skips the nixpkgs
// override and relies on the ambient nixos-unstable resolution).
func ossNixpkgsRev(ossDir string) string {
	b, err := os.ReadFile(filepath.Join(ossDir, "flake.lock"))
	if err != nil {
		return ""
	}
	// Cheap, dependency-free scrape: find the nixpkgs node's "rev". The lock is
	// small and stable-shaped; a full JSON decode would work too but this keeps
	// the test import-light.
	s := string(b)
	i := strings.Index(s, `"nixpkgs"`)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], `"rev":`)
	if j < 0 {
		return ""
	}
	rest := s[i+j+len(`"rev":`):]
	q1 := strings.IndexByte(rest, '"')
	if q1 < 0 {
		return ""
	}
	rest = rest[q1+1:]
	q2 := strings.IndexByte(rest, '"')
	if q2 < 0 {
		return ""
	}
	return rest[:q2]
}

// evalRiftAttr runs `nix eval <dir>#<attr>` with the given extra
// `--override-input` pairs (plus any extra args, e.g. `--json`), returning
// combined output. The `rift` input is always overridden to the local oss by
// the caller.
func evalRiftAttr(t *testing.T, dir, attr string, overrides [][2]string, extra ...string) (string, error) {
	t.Helper()
	args := []string{
		"eval",
		"--extra-experimental-features", "nix-command flakes",
	}
	args = append(args, extra...)
	for _, o := range overrides {
		args = append(args, "--override-input", o[0], o[1])
	}
	args = append(args, dir+"#"+attr)
	cmd := exec.Command("nix", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// evalRiftImageDrvPath evals `<dir>#fixed-labs.rift.image.drvPath` — forces the
// derivation to WHNF (eval-only, never builds) — via evalRiftAttr.
func evalRiftImageDrvPath(t *testing.T, dir string, overrides [][2]string) (string, error) {
	t.Helper()
	return evalRiftAttr(t, dir, "fixed-labs.rift.image.drvPath", overrides)
}

// nixEvalPreamble applies the shared environment guards used by the nix-gated
// eval tests: RIFT_REQUIRE_NIX=1 turns a would-be skip into a hard failure (the
// dedicated nix CI lane sets it so these tests are REAL gates there), while
// off-CI (and the no-nix Pants RBE lane) they skip cleanly instead of failing a
// hermetic sandbox. Returns the located oss flake dir; on a skip it does not
// return (the test has already been skipped/failed via t).
func nixEvalPreamble(t *testing.T) string {
	t.Helper()
	requireNix := os.Getenv("RIFT_REQUIRE_NIX") == "1"
	skipOrFatal := func(reason string) {
		if requireNix {
			t.Fatalf("RIFT_REQUIRE_NIX=1 but %s", reason)
		}
		t.Skip(reason)
	}
	if _, err := exec.LookPath("nix"); err != nil {
		skipOrFatal("nix not on PATH; the deterministic spine runs on dev/CI machines with nix")
	}
	ossDir := ossFlakeDir(t)
	if ossDir == "" {
		skipOrFatal("local oss flake not locatable (set RIFT_OSS_FLAKE, or run from within the repo)")
	}
	return ossDir
}

func TestInitEmitEvalSpine(t *testing.T) {
	ossDir := nixEvalPreamble(t)
	riftOverride := [2]string{"rift", "path:" + ossDir}

	// (a) Node and (b) Python-with-a-system-library: splice emit's two pieces
	// into the from-scratch skeleton, then eval.
	skeletonFixtures := []struct {
		name     string
		packages []string
		options  []kv
	}{
		{
			name:     "node",
			packages: []string{"nodejs"},
		},
		{
			name:     "python-syslib",
			packages: []string{"python3", "zlib"}, // zlib = a system C library
			options:  []kv{{k: "loginUser", v: `"dev"`}},
		},
	}
	for _, f := range skeletonFixtures {
		f := f
		t.Run(f.name, func(t *testing.T) {
			emit, err := renderEmit(f.packages, f.options)
			if err != nil {
				t.Fatalf("renderEmit(%v, %v): %v", f.packages, f.options, err)
			}
			flake := spliceSkeleton(t, emit)
			tmp := t.TempDir()
			if err := os.WriteFile(filepath.Join(tmp, "flake.nix"), []byte(flake), 0o644); err != nil {
				t.Fatalf("write flake.nix: %v", err)
			}
			out, err := evalRiftImageDrvPath(t, tmp, [][2]string{riftOverride})
			if err != nil {
				t.Fatalf("nix eval failed for fixture %q\n--- flake.nix ---\n%s\n--- nix output ---\n%s", f.name, flake, out)
			}
			if !strings.Contains(out, ".drv") {
				t.Fatalf("fixture %q: expected a .drv path in eval output, got:\n%s", f.name, out)
			}

			// INV-2 — the versioned-envelope contract: `fixed-labs.rift` is
			// `{ version = <N>; image = …; }` and the managed builder reads
			// `version` to decide how to interpret the envelope. Pin it to
			// EXACTLY 1 here (same spliced flake, same rift override), so a
			// bump to the envelope shape can't slip through unversioned. Only
			// the node fixture asserts this — one proof of the contract is
			// enough; every fixture routes through the same mkRift.
			if f.name == "node" {
				vout, err := evalRiftAttr(t, tmp, "fixed-labs.rift.version", [][2]string{riftOverride}, "--json")
				if err != nil {
					t.Fatalf("nix eval of fixed-labs.rift.version failed for fixture %q\n--- flake.nix ---\n%s\n--- nix output ---\n%s", f.name, flake, vout)
				}
				// The eval value is the LAST non-empty line: nix may prepend a
				// lock-file `warning:` to combined stdout+stderr (as the drvPath
				// check tolerates). `--json` renders the integer as bare `1`.
				if got := lastLine(vout); got != "1" {
					t.Fatalf("fixed-labs.rift.version must be exactly 1 (INV-2), got %q\n--- full nix output ---\n%s", got, vout)
				}
			}
		})
	}

	// (c) existing-flake: eval the checked-in expected-merged/flake.nix (the
	// hand-merged result). Do NOT re-run the merge. Its independent `nixpkgs`
	// input is overridden to oss's pinned rev so the eval needs no extra fetch.
	t.Run("existing-flake", func(t *testing.T) {
		_, thisFile, _, ok := runtime.Caller(0)
		if !ok {
			t.Skip("cannot resolve test source path to find the fixture")
		}
		merged := filepath.Join(filepath.Dir(thisFile), "testdata", "init-fixtures", "existing-flake", "expected-merged", "flake.nix")
		src, err := os.ReadFile(merged)
		if err != nil {
			t.Fatalf("read expected-merged fixture %q: %v", merged, err)
		}
		tmp := t.TempDir()
		if err := os.WriteFile(filepath.Join(tmp, "flake.nix"), src, 0o644); err != nil {
			t.Fatalf("write flake.nix: %v", err)
		}
		overrides := [][2]string{riftOverride}
		if rev := ossNixpkgsRev(ossDir); rev != "" {
			overrides = append(overrides, [2]string{"nixpkgs", "github:NixOS/nixpkgs/" + rev})
		}
		out, err := evalRiftImageDrvPath(t, tmp, overrides)
		if err != nil {
			t.Fatalf("nix eval failed for existing-flake expected-merged\n--- flake.nix ---\n%s\n--- nix output ---\n%s", string(src), out)
		}
		if !strings.Contains(out, ".drv") {
			t.Fatalf("existing-flake: expected a .drv path in eval output, got:\n%s", out)
		}
	})
}

// TestMkRiftForwardsRepoSrcBake DIFFERENTIALLY guards mkRift's whole value-add:
// the hand-written `// { inherit repoSrc imageCommit; }` re-injection in
// oss/default.nix's lib.mkRift. That splice re-forwards the DEFAULTED args
// (`repoSrc ? self`) to mkDevimage — Nix's `@args` binds only the *passed*
// attrs, not the defaulted ones, so without the splice mkDevimage would see no
// repoSrc and fall back to its own `repoSrc ? null`, baking NOTHING. Crucially
// that broken image STILL evaluates to a valid derivation, so the eval-spine
// (which only asserts `.image.drvPath` evaluates) passes green while boxes boot
// with no code checked out.
//
// The proof is differential: expose two mkRift envelopes over the SAME `self`,
// one letting repoSrc default (→ self, bakes the tmp flake dir as the tree) and
// one passing `repoSrc = null` explicitly (bakes nothing). With the re-injection
// present the two forward DIFFERENT repoSrc values, so the two image
// derivations differ → distinct drvPaths. If the `//` re-injection were dropped,
// BOTH envelopes would forward null (defaulted args lost) → IDENTICAL drvPaths
// → this test fails. So `baked.drvPath != bare.drvPath` is exactly the guard.
//
// `self` resolves to the temp flake dir; that dir is a real path (it holds
// flake.nix), so `repoSrc = self` bakes a non-empty tree, distinct from the null
// case. No repo files beyond flake.nix are needed — only the drvPaths' identity
// is under test, and it's eval-only (`.drvPath` forces WHNF, never builds).
func TestMkRiftForwardsRepoSrcBake(t *testing.T) {
	ossDir := nixEvalPreamble(t)
	riftOverride := [2]string{"rift", "path:" + ossDir}

	// Two mkRift envelopes over the same `self`: `baked` lets repoSrc default to
	// self (bakes the tree); `bare` passes repoSrc = null (bakes nothing).
	flake := `{
  inputs.rift.url = "github:fixed-labs/oss";
  outputs = { self, rift, ... }: {
    baked = rift.lib.mkRift { inherit self; };
    bare  = rift.lib.mkRift { inherit self; repoSrc = null; };
  };
}
`
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "flake.nix"), []byte(flake), 0o644); err != nil {
		t.Fatalf("write flake.nix: %v", err)
	}

	evalDrv := func(attr string) string {
		out, err := evalRiftAttr(t, tmp, attr+".image.drvPath", [][2]string{riftOverride})
		if err != nil {
			t.Fatalf("nix eval of %s.image.drvPath failed\n--- flake.nix ---\n%s\n--- nix output ---\n%s", attr, flake, out)
		}
		drv := lastLine(out)
		if !strings.Contains(drv, ".drv") {
			t.Fatalf("%s: expected a .drv path in eval output, got:\n%s", attr, out)
		}
		return drv
	}

	baked := evalDrv("baked")
	bare := evalDrv("bare")
	t.Logf("baked.image.drvPath = %s", baked)
	t.Logf("bare.image.drvPath  = %s", bare)

	if baked == bare {
		t.Fatalf("mkRift dropped the repoSrc re-injection: baked (repoSrc=self) and "+
			"bare (repoSrc=null) produced IDENTICAL drvPaths (%s), meaning the "+
			"defaulted repoSrc was NOT forwarded to mkDevimage — boxes would boot "+
			"with no code baked in", baked)
	}
}
