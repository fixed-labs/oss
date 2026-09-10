package main

// bootstrap_eval_test.go — FIX-324 T17: nix-gated STRUCTURE test of the two
// rendered image init scripts (bootstrap and base) and of the baked system's
// agent-unit wiring. The bash supervisor/ladder/realizer RUNTIME behavior is
// not runnable in CI (no Fly-style machine harness); this eval-level test is
// the recorded coverage strategy for it, together with the `bash -n` syntax
// gate below (the only one anywhere — writeScript performs none) and the
// design's own post-deploy staging validation.
//
// Eval-only: `.initScript.text` and the `.baseSystem` passthru are ordinary
// attributes — nothing here builds an image. Same gating as
// init_eval_test.go / devboxes_base_eval_test.go: skips cleanly without nix,
// hard-fails under RIFT_REQUIRE_NIX=1 (the rift-eval-spine lane).
//
// What a failure catches: the shared-init regression class — a client image
// gaining the nix closure; the bootstrap losing its realizer/supervisor
// machinery; boot-env allowlist drift breaking the PR-7 producer seam;
// key-baking silently dropped (the flag-day realization-fails-everywhere
// hazard); an unparseable init bricking every image; a dropped xtrace
// bracket leaking the workspace bearer token into fly logs.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// T17(e) — the three platform cache-signing PUBLIC keys, as literals. The
// probe flake below passes these ITSELF as `cacheTrustedPublicKeys`, so leg
// (e) can only assert its own inputs back through mkDevimage's interpolation
// (plus cache.nixos.org-1, which mkDevimage adds on its own — that half is a
// real wiring pin, not an echo). The ROOT flake's actual bootstrap-image
// wiring (`bootstrap = true` + `cacheTrustedPublicKeys =
// import ./nix/rift-cache-public-keys.nix`) is pinned by the monorepo's
// rift-eval-spine CI step (T16), not here. T17(e)'s residual value is
// attribution plus STANDALONE coverage in the published oss subtree (the OSS
// mirror), where that CI step does not exist — recorded so a future de-dup
// pass doesn't cut the wrong one.
var riftCacheProbeKeys = []string{
	"fpl-rift-dev-1:jfE5fwR77xQb9C92asmMg/dKr8aJZ79amngKjiADtsk=",
	"fpl-rift-staging-1:RplA4LYrSipcOfxxioPXjbKuVUwVovrL6fL9jql/3xw=",
	"fpl-rift-prod-1:Pslx4Ec/XriX0R5u6qfqtvOKVQ5+YUlqUDUvp1pXvZM=",
}

// cache.nixos.org's well-known key — hardcoded in mkDevimage as the realizer
// trust anchor for the public paths the builder deliberately does not copy
// into the tenant prefix. A literal here (never read from the source under
// test) so corruption of the baked constant is caught.
const cacheNixosKey = "cache.nixos.org-1:6NCHdD59X431o0gWypbMrAURkbJ16ZPMQFGspcDShjY="

// T17(d) — the §3.5 boot-env allowlist: the EXACT name set the writer loop
// must capture into /etc/devboxes/boot-env. Cross-process contract literals
// (systemd EnvironmentFile= on the consumer side); order and line wrapping
// are deliberately NOT part of the contract.
var bootEnvAllowlist = []string{
	"RIFT_WORKSPACE_ID", "RIFT_API_URL", "RIFT_TOKEN", "RIFT_WG_IP",
	"RIFT_RELAY_ENDPOINT", "SYSTEM_PATH", "RIFT_BK", "RIFT_REVOKE_EPOCH",
	"RIFT_CACHE_ENDPOINT", "RIFT_CACHE_PREFIX", "RIFT_CACHE_KEY_ID",
	"RIFT_CACHE_SECRET", "RIFT_REPO_URL", "RIFT_REPO_REF",
	"RIFT_REPO_COMMIT", "RIFT_REPO_DIR",
}

// bootstrapProbe carries the eval results: both rendered init scripts and
// whether each image's BAKED system ships the in-system agent unit.
type bootstrapProbe struct {
	BootstrapInit          string `json:"bootstrapInit"`
	BaseInit               string `json:"baseInit"`
	BootstrapAgentInSystem bool   `json:"bootstrapAgentInSystem"`
	BaseAgentInSystem      bool   `json:"baseAgentInSystem"`
}

func evalBootstrapProbe(t *testing.T, ossDir string) bootstrapProbe {
	t.Helper()
	keys := make([]string, len(riftCacheProbeKeys))
	for i, k := range riftCacheProbeKeys {
		keys[i] = fmt.Sprintf("%q", k)
	}
	// `.baseSystem` is mkDevimage's eval-only test seam (passthru, see
	// oss/default.nix): the baked system's own module eval, so the
	// agent-unit presence checks exercise the REAL
	// `bootstrap = true ⇒ rift.internal.agentInSystem = false` wiring
	// rather than a re-built lookalike system.
	flake := fmt.Sprintf(`{
  inputs.rift.url = "github:fixed-labs/oss";
  outputs = { self, rift, ... }:
    let
      bootstrap = rift.lib.mkDevimage {
        bootstrap = true;
        cacheTrustedPublicKeys = [ %s ];
      };
      base = rift.lib.mkDevimage { };
    in {
      probe = {
        bootstrapInit = bootstrap.initScript.text;
        baseInit = base.initScript.text;
        bootstrapAgentInSystem = bootstrap.baseSystem.config.systemd.units ? "devboxes-agent.service";
        baseAgentInSystem = base.baseSystem.config.systemd.units ? "devboxes-agent.service";
      };
    };
}
`, strings.Join(keys, " "))
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "flake.nix"), []byte(flake), 0o644); err != nil {
		t.Fatalf("write flake.nix: %v", err)
	}
	out, err := evalRiftAttr(t, tmp, "probe", [][2]string{{"rift", "path:" + ossDir}}, "--json")
	if err != nil {
		t.Fatalf("nix eval of the bootstrap probe failed\n--- flake.nix ---\n%s\n--- nix output ---\n%s", flake, out)
	}
	var p bootstrapProbe
	if err := json.Unmarshal([]byte(lastLine(out)), &p); err != nil {
		t.Fatalf("parse nix eval --json output: %v\n--- nix output ---\n%s", err, out)
	}
	if p.BootstrapInit == "" || p.BaseInit == "" {
		t.Fatalf("probe returned an empty init render (bootstrap=%d bytes, base=%d bytes) — nothing below can assert anything real",
			len(p.BootstrapInit), len(p.BaseInit))
	}
	return p
}

// mustIndex returns the byte offset of needle in the render, failing the test
// with attribution when the anchor is gone.
func mustIndex(t *testing.T, render, needle, what string) int {
	t.Helper()
	i := strings.Index(render, needle)
	if i < 0 {
		t.Fatalf("%s: anchor %q not found in the rendered init", what, needle)
	}
	return i
}

// parseBootEnvNames extracts the SET of names the boot-env writer loop
// iterates, tolerant of whitespace and `\`-line-continuations (the contract
// is the name set; order and wrapping are the emitter's business). The block
// is located by its output redirect — the one contract-bearing literal — not
// by the loop variable's name.
func parseBootEnvNames(t *testing.T, render string) map[string]bool {
	t.Helper()
	const sink = "> /newroot/etc/devboxes/boot-env"
	end := strings.Index(render, sink)
	if end < 0 {
		t.Fatalf("boot-env writer block not found: no %q redirect in the render", sink)
	}
	head := strings.LastIndex(render[:end], "for ")
	if head < 0 {
		t.Fatalf("boot-env writer block has no `for` loop before its %q redirect", sink)
	}
	rel := strings.Index(render[head:end], "; do")
	if rel < 0 {
		t.Fatalf("boot-env writer `for` header has no terminating `; do`")
	}
	header := strings.ReplaceAll(render[head:head+rel], "\\\n", " ")
	fields := strings.Fields(header)
	if len(fields) < 4 || fields[0] != "for" || fields[2] != "in" {
		t.Fatalf("boot-env writer header did not parse as `for <var> in <names…>`: %q", header)
	}
	names := make(map[string]bool, len(fields)-3)
	for _, n := range fields[3:] {
		names[n] = true
	}
	return names
}

// xtraceToggle is one `set +x` / `set -x` COMMAND line (comments that merely
// mention the toggles do not count — 3 of the 6 textual `set +x` occurrences
// in the bootstrap render are prose).
type xtraceToggle struct {
	off     int
	traceOn bool
}

// xtraceToggles scans the render line-by-line for toggle command lines. The
// scan is LINEAR: it deliberately ignores subshell scoping, which is exactly
// why no anchor may be placed after the agent-start subshell's un-closed
// `set +x` (see the leg-(g) comment below).
func xtraceToggles(render string) []xtraceToggle {
	var toggles []xtraceToggle
	off := 0
	for _, line := range strings.SplitAfter(render, "\n") {
		switch strings.TrimSpace(strings.TrimSuffix(line, "\n")) {
		case "set +x":
			toggles = append(toggles, xtraceToggle{off, false})
		case "set -x":
			toggles = append(toggles, xtraceToggle{off, true})
		}
		off += len(line)
	}
	return toggles
}

// xtraceOnAt reports the effective xtrace state at a byte offset under the
// linear scan. Initial state is ON: the script opens with `set -eux` (asserted
// as the leg's arming anchor — without it there is no leak hazard at all).
func xtraceOnAt(toggles []xtraceToggle, off int) bool {
	on := true
	for _, tg := range toggles {
		if tg.off >= off {
			break
		}
		on = tg.traceOn
	}
	return on
}

func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// TestBootstrapImageStructure is T17: one probe eval, seven legs.
func TestBootstrapImageStructure(t *testing.T) {
	ossDir := nixEvalPreamble(t)
	p := evalBootstrapProbe(t, ossDir)

	// --- (a) bootstrap render: the machinery the whole design hangs off ---
	t.Run("bootstrap init carries realizer, supervisor, and agent machinery", func(t *testing.T) {
		for _, a := range []struct{ what, needle string }{
			// Rung-1 profile resolution: the volume profile is the sole
			// source of truth ("profile set" ≡ "realization completed").
			{"rung-1 profile existence check", "-L /persist/nix/var/nix/profiles/system"},
			{"rung-1 selection", "BOOT_RUNG=profile"},
			// The realizer stage: substitution onto the volume CHROOT store.
			{"realizer substitution of $SYSTEM_PATH", `--realise "$SYSTEM_PATH"`},
			{"realizer volume chroot store", "local?root=/persist"},
			// The producer half of the /run/rift file contract T8–T10's Go
			// consumers read (their literals are pinned Go-side in T8(f);
			// /persist/rift/agent is agent-only and deliberately NOT here —
			// the init never writes it).
			{"boot-rung writer", "> /run/rift/boot-rung"},
			{"sd-pid writer", "> /run/rift/sd-pid"},
			// The SIGUSR1 recovery request persists its one-shot flag FIRST
			// (crash-safe ordering).
			{"recovery-inflight persist", "touch /persist/rift/recovery-inflight"},
			// The agent, OUTSIDE the client system (INV-5).
			{"agent-start bootstrap flag", "export RIFT_BOOTSTRAP=1"},
		} {
			if !strings.Contains(p.BootstrapInit, a.needle) {
				t.Errorf("bootstrap init lost its %s (%q)", a.what, a.needle)
			}
		}
		// NIX_REMOTE cleared for the realizer: boot.isContainer sets
		// NIX_REMOTE=daemon and no daemon exists pre-systemd — inherited, every
		// substitution hangs forever. `NIX_REMOTE=` followed by whitespace is
		// the cleared-assignment shape (any value would append to the match).
		if !regexp.MustCompile(`NIX_REMOTE=[ \t\n]`).MatchString(p.BootstrapInit) {
			t.Errorf("bootstrap init no longer clears NIX_REMOTE for the realizer — every substitution hangs against a daemon socket that never appears")
		}
		// USR1 trap wiring: presence of `trap <handler> USR1`. The handler's
		// shell-function NAME is incidental and deliberately not pinned.
		if !regexp.MustCompile(`trap +\S+ +USR1`).MatchString(p.BootstrapInit) {
			t.Errorf("bootstrap init has no `trap … USR1` — the agent's recovery request lands on a supervisor that ignores it")
		}
		if !regexp.MustCompile(`exec \S+/bin/devboxes-agent`).MatchString(p.BootstrapInit) {
			t.Errorf("bootstrap init never execs the agent binary — no agent outside the client system (INV-5)")
		}
		// The marker-clear line: without a /run tmpfs the markers land in the
		// overlay upper and persist across machine boots, so each boot must
		// clear last boot's set. One rm line, all four markers, any order.
		var clearLine string
		for _, line := range strings.Split(p.BootstrapInit, "\n") {
			lt := strings.TrimSpace(line)
			if strings.HasPrefix(lt, "rm -f ") && strings.Contains(lt, "/run/rift/") {
				clearLine = lt
				break
			}
		}
		if clearLine == "" {
			t.Errorf("bootstrap init has no `rm -f …/run/rift/…` marker-clear line — stale markers persist via the overlay upper across boots")
		} else {
			for _, marker := range []string{
				"/run/rift/recovery-boot", "/run/rift/sd-pid",
				"/run/rift/realization-status", "/run/rift/boot-rung",
			} {
				found := false
				for _, f := range strings.Fields(clearLine) {
					if f == marker {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("marker-clear line does not clear %s (line: %q) — last boot's marker survives into this boot", marker, clearLine)
				}
			}
		}
	})

	// --- (b) base render: shared supervisor kept, bootstrap stages absent ---
	t.Run("base init keeps the shared supervisor and none of the bootstrap stages", func(t *testing.T) {
		// POSITIVE anchors FIRST: prove the render is real and the SHARED
		// supervisor machinery ships in every image. These sit outside
		// `optionalString bootstrap` by design (§3.7's exit-status fix applies
		// to every image); the natural refactor error — wrapping the
		// supervisor block into the bootstrap conditional — would strip the
		// ladder and exit-status fix from every client image, and without
		// these anchors the negatives below would pass on an empty or
		// mis-addressed render. (The `status` variable's name is incidental
		// and deliberately not pinned.)
		if !strings.Contains(p.BaseInit, "> /newroot/etc/devboxes/boot-env") {
			t.Fatalf("base init lost the boot-env writer (output path /newroot/etc/devboxes/boot-env) — the render is not the shared entrypoint; negatives below would be vacuous")
		}
		if !strings.Contains(p.BaseInit, "pivot_root . oldroot") {
			t.Fatalf("base init lost the pivot_root handoff — not the shared entrypoint; negatives below would be vacuous")
		}
		if !strings.Contains(p.BaseInit, "> /run/rift/boot-rung") {
			t.Errorf("base init lost the /run/rift/boot-rung writer — the shared two-stage supervisor was stripped from client images (likely wrapped into the bootstrap conditional)")
		}
		if !regexp.MustCompile(`trap +\S+ +USR1`).MatchString(p.BaseInit) {
			t.Errorf("base init lost its `trap … USR1` — the shared supervisor was stripped from client images (likely wrapped into the bootstrap conditional)")
		}

		// Negatives, each ARMED against the bootstrap render first: a pattern
		// that fails to match the sibling render in which the guarded text
		// provably exists cannot prove absence here (it may just match
		// nothing anywhere).
		for _, n := range []struct {
			what string
			re   *regexp.Regexp
			why  string
		}{
			{"realizer substitution", regexp.MustCompile(`--realise`),
				"client images must not substitute systems at boot"},
			{"volume chroot store", regexp.MustCompile(regexp.QuoteMeta("local?root=/persist")),
				"client images must not touch the volume store as a chroot store"},
			{"outside-agent start", regexp.MustCompile(`RIFT_BOOTSTRAP=1`),
				"a client image starting the outside agent would run two agents per box"},
			{"agent binary exec", regexp.MustCompile(`/bin/devboxes-agent`),
				"the base init must not reference the agent binary (in-system unit owns it)"},
			{"nix interpolation (closure-bloat guard)", regexp.MustCompile(`/nix/store/[a-z0-9]+-nix-`),
				"a nix store-path reference drags the whole nix package into every client image's closure"},
		} {
			if !n.re.MatchString(p.BootstrapInit) {
				t.Errorf("armed control broken: %s pattern %q does not match the BOOTSTRAP render — it cannot prove absence from the base render", n.what, n.re)
				continue
			}
			if loc := n.re.FindString(p.BaseInit); loc != "" {
				t.Errorf("base init gained the bootstrap-only %s (%q) — %s", n.what, loc, n.why)
			}
		}
	})

	// --- (c) rift.internal.agentInSystem, via the baseSystem eval seam ---
	t.Run("agent unit present in the default baked system, absent for bootstrap", func(t *testing.T) {
		if !p.BaseAgentInSystem {
			t.Errorf("the DEFAULT image's baked system no longer ships devboxes-agent.service — every existing client image loses its agent on `nix flake update`")
		}
		if p.BootstrapAgentInSystem {
			t.Errorf("the BOOTSTRAP image's baked system still ships devboxes-agent.service — a recovery boot runs two agents for one workspace (INV-5 broken)")
		}
	})

	// --- (d) the boot-env allowlist, as a set, on BOTH renders ---
	t.Run("boot-env writer names exactly the 16 allowlist vars", func(t *testing.T) {
		want := make(map[string]bool, len(bootEnvAllowlist))
		for _, n := range bootEnvAllowlist {
			want[n] = true
		}
		for render, text := range map[string]string{
			"bootstrap": p.BootstrapInit,
			"base":      p.BaseInit,
		} {
			got := parseBootEnvNames(t, text)
			for _, n := range bootEnvAllowlist {
				if !got[n] {
					t.Errorf("%s render: boot-env writer dropped %s — consumers read an empty value where the launch env set one", render, n)
				}
			}
			for _, n := range sortedNames(got) {
				if !want[n] {
					t.Errorf("%s render: boot-env writer captures %s, which is not on the §3.5 allowlist — machine env leaks into the box beyond the contract", render, n)
				}
			}
		}
	})

	// --- (e) realizer trusted keys (see riftCacheProbeKeys' comment for why
	// this asserts the probe's own inputs, and what its residual value is) ---
	t.Run("realizer trusted-public-keys carry the three fpl keys and cache.nixos.org-1", func(t *testing.T) {
		m := regexp.MustCompile(`trusted-public-keys '([^']*)'`).FindStringSubmatch(p.BootstrapInit)
		if m == nil {
			t.Fatalf("bootstrap init has no `--option trusted-public-keys '…'` — the realizer verifies signatures against nothing (or require-sigs was dropped)")
		}
		keySet := make(map[string]bool)
		for _, k := range strings.Fields(m[1]) {
			keySet[k] = true
		}
		for _, k := range append([]string{cacheNixosKey}, riftCacheProbeKeys...) {
			if !keySet[k] {
				t.Errorf("realizer trusted-public-keys lost %q — substitution of paths signed by it fails on every launch\n--- interpolation ---\n%s", k, m[1])
			}
		}
	})

	// --- (f) bash -n on BOTH rendered inits — the only syntax gate for the
	// embedded script anywhere (writeScript performs none) ---
	t.Run("bash -n passes on both rendered inits", func(t *testing.T) {
		bash, err := exec.LookPath("bash")
		if err != nil {
			if os.Getenv("RIFT_REQUIRE_NIX") == "1" {
				t.Fatalf("RIFT_REQUIRE_NIX=1 but bash not on PATH; the syntax gate cannot run")
			}
			t.Skip("bash not on PATH; skipping the syntax gate")
		}
		for name, text := range map[string]string{
			"bootstrap": p.BootstrapInit,
			"base":      p.BaseInit,
		} {
			script := filepath.Join(t.TempDir(), name+"-init.sh")
			if err := os.WriteFile(script, []byte(text), 0o644); err != nil {
				t.Fatalf("write %s render: %v", name, err)
			}
			if out, err := exec.Command(bash, "-n", script).CombinedOutput(); err != nil {
				t.Errorf("`bash -n` rejects the %s init (%v) — every image built from this render fails at its very first boot line\n--- bash -n ---\n%s", name, err, out)
			}
		}
	})

	// --- (g) secret-hygiene xtrace brackets ---
	// The script runs under `set -eux`; a dropped bracket prints the bearer
	// token / cache read credential into fly logs with everything else green.
	// STATE-based, not existence-based: a linear scan of the toggle COMMAND
	// lines must yield state `+x` (trace off) at each of the three secret
	// sites and `-x` (trace on) at two interior functional anchors between
	// them — the interior anchors are what prove bracket LOCALITY (a dropped
	// site-local `set +x` or a dropped interior close both flip an asserted
	// state). Deliberately NOT asserted: any toggle after site (3) — the
	// agent-start block is an exec-terminated subshell that never re-enables
	// xtrace, so correct code has no toggle there and demanding one would
	// only be satisfiable by dead code. For the same reason no anchor may sit
	// after site (3): the linear scan reads `+x` there on correct code (the
	// subshell's `set +x` is never closed), so e.g. the boot-ladder
	// rung-selection is NOT a valid `-x` anchor.
	t.Run("xtrace off at the three secret sites, on at the interior anchors", func(t *testing.T) {
		// Arming anchor: xtrace is enabled at all. Without `set -eux` there
		// is no leak hazard and the bracket states below prove nothing.
		for name, text := range map[string]string{
			"bootstrap": p.BootstrapInit,
			"base":      p.BaseInit,
		} {
			if !strings.Contains(text, "set -eux") {
				t.Fatalf("%s init no longer runs under `set -eux` — the xtrace-bracket scan below is unarmed (and boot tracing for fly logs is gone)", name)
			}
		}

		boot := xtraceToggles(p.BootstrapInit)
		// Site anchors, in render order. `AWS_SECRET_ACCESS_KEY="` (with the
		// opening quote) and `. /etc/devboxes/boot-env` (with the sourcing
		// dot) are chosen over the bare names because prose comments in the
		// render mention both.
		sites := []struct {
			what    string
			needle  string
			wantOn  bool
			leaking string
		}{
			{"secret site 1: boot-env writer", "> /newroot/etc/devboxes/boot-env", false,
				"the writer's expanded printf prints the bearer token into fly logs"},
			{"interior anchor: pivot_root handoff", "pivot_root . oldroot", true,
				"the writer's closing `set -x` was dropped — the bracket is no longer local"},
			{"secret site 2: realizer credential invocation", `AWS_SECRET_ACCESS_KEY="`, false,
				"the expanded AWS_SECRET_ACCESS_KEY=… prefix prints the cache read credential into fly logs"},
			{"interior anchor: realization-status reason writer", `echo "reason=`, true,
				"the realizer's closing `set -x` was dropped — the bracket is no longer local"},
			{"secret site 3: agent-start boot-env sourcing", ". /etc/devboxes/boot-env", false,
				"tracing the sourced boot-env prints the bearer token line by line into fly logs"},
		}
		prev := -1
		for _, s := range sites {
			off := mustIndex(t, p.BootstrapInit, s.needle, s.what)
			if off <= prev {
				t.Fatalf("%s (%q at %d) does not follow the previous anchor (%d) — the bracket-locality argument assumes this order; re-verify the anchors against the render", s.what, s.needle, off, prev)
			}
			prev = off
			if got := xtraceOnAt(boot, off); got != s.wantOn {
				state := map[bool]string{true: "-x (tracing)", false: "+x (quiet)"}
				t.Errorf("bootstrap init: xtrace state at %s is %s, want %s — %s", s.what, state[got], state[s.wantOn], s.leaking)
			}
		}

		// The base render shares the writer: assert site 1 and its following
		// interior anchor there too (it has no realizer or agent sites).
		base := xtraceToggles(p.BaseInit)
		writerOff := mustIndex(t, p.BaseInit, "> /newroot/etc/devboxes/boot-env", "base secret site: boot-env writer")
		if xtraceOnAt(base, writerOff) {
			t.Errorf("base init: xtrace is ON at the boot-env writer — the writer's expanded printf prints the bearer token into fly logs on every client image")
		}
		pivotOff := mustIndex(t, p.BaseInit, "pivot_root . oldroot", "base interior anchor: pivot_root handoff")
		if writerOff >= pivotOff {
			t.Fatalf("base init: pivot_root (%d) does not follow the boot-env writer (%d) — re-verify the anchors against the render", pivotOff, writerOff)
		}
		if !xtraceOnAt(base, pivotOff) {
			t.Errorf("base init: xtrace is OFF at pivot_root — the writer's closing `set -x` was dropped; the bracket is no longer local")
		}
	})
}
