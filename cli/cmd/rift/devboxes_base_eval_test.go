package main

// devboxes_base_eval_test.go — nix-gated proof of the boot-DNS re-assert that
// keeps a cold-booted box able to resolve the control plane (FIX-321). The
// mechanism and the two candidate root causes are documented on the
// devboxes-resolv unit in `oss/nix/devboxes-base/module.nix`.
//
// Guards the properties whose loss reproduces the outage SILENTLY — a capture
// that stops firing turns the unit into a clean ConditionFileNotEmpty skip, and
// a payload that stops registering leaves the wiring looking perfect. Both are
// invisible in a passing CI run and cost 15 minutes of unreachable box.
//
// Eval-only: `.text` on a writeScript and `.script` on a unit are ordinary
// attributes, so nothing here builds an image. Same gating as
// init_eval_test.go — skips cleanly without nix, hard-fails under
// RIFT_REQUIRE_NIX=1 (the rift-eval-spine lane).

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// directiveHasToken reports whether a rendered systemd unit carries `token` in
// its `directive=` line. Per-token rather than whole-line, so adding a second
// entry (nixpkgs space-joins list options) is not a false failure.
func directiveHasToken(unit, directive, token string) bool {
	for _, line := range strings.Split(unit, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), directive+"=")
		if !ok {
			continue
		}
		for _, f := range strings.Fields(rest) {
			if f == token {
				return true
			}
		}
	}
	return false
}

// devboxesBaseProbe evaluates the base system and the image, returning the
// pieces of the FIX-321 wiring under test.
type devboxesBaseProbe struct {
	ResolvUnit   string `json:"resolvUnit"`
	AgentUnit    string `json:"agentUnit"`
	ResolvScript string `json:"resolvScript"`
	Entrypoint   string `json:"entrypoint"`
	Capture      string `json:"capture"`
}

func evalDevboxesBaseProbe(t *testing.T, ossDir string) devboxesBaseProbe {
	t.Helper()
	// `hello` stands in for the agent: only the wiring is under test.
	flake := `{
  inputs.rift.url = "github:fixed-labs/oss";
  outputs = { self, rift, ... }:
    let
      sys = rift.inputs.nixpkgs.lib.nixosSystem {
        system = "x86_64-linux";
        modules = [
          rift.nixosModules.devboxes-base
          {
            rift.devboxes-base.agentPackage =
              (import rift.inputs.nixpkgs { system = "x86_64-linux"; }).hello;
          }
        ];
      };
      image = rift.lib.mkDevimage { };
    in {
      probe = {
        resolvUnit = sys.config.systemd.units."devboxes-resolv.service".text;
        agentUnit = sys.config.systemd.units."devboxes-agent.service".text;
        resolvScript = sys.config.systemd.services.devboxes-resolv.script;
        entrypoint = image.initScript.text;
        capture = image.captureBootResolv;
      };
    };
}
`
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "flake.nix"), []byte(flake), 0o644); err != nil {
		t.Fatalf("write flake.nix: %v", err)
	}
	out, err := evalRiftAttr(t, tmp, "probe", [][2]string{{"rift", "path:" + ossDir}}, "--json")
	if err != nil {
		t.Fatalf("nix eval of the devboxes-base probe failed\n--- flake.nix ---\n%s\n--- nix output ---\n%s", flake, out)
	}
	var p devboxesBaseProbe
	if err := json.Unmarshal([]byte(lastLine(out)), &p); err != nil {
		t.Fatalf("parse nix eval --json output: %v\n--- nix output ---\n%s", err, out)
	}
	return p
}

// mkFifo creates a named pipe with no writer — the shape that would block the
// entrypoint's `read` forever if the `-f` guard regressed.
func mkFifo(path string) error { return syscall.Mkfifo(path, 0o644) }

// capturePath is the single literal the entrypoint writes, the unit reads on
// stdin, and ConditionFileNotEmpty gates on. Pinning all three to one constant
// is the point: move the path in two of those places and the box silently stops
// being fixed, with every other assertion still green.
const capturePath = "/etc/devboxes/boot-resolv.conf"

// TestDevboxesBaseBootDNS covers the whole FIX-321 chain from one probe eval:
// the capture is spliced into the entrypoint between the mkdir that creates its
// output directory and the pivot that removes it, it behaves correctly across
// the boot shapes, and the unit that consumes it is wired and ordered.
func TestDevboxesBaseBootDNS(t *testing.T) {
	ossDir := nixEvalPreamble(t)
	p := evalDevboxesBaseProbe(t, ossDir)

	spliceAt := strings.Index(p.Entrypoint, p.Capture)
	if spliceAt < 0 {
		t.Fatalf("the capture snippet is no longer spliced into the image entrypoint — nothing\n"+
			"writes the file devboxes-resolv re-asserts, so the unit skips and the box stays\n"+
			"unreachable\n--- capture ---\n%s", p.Capture)
	}
	// …and after the mkdir that creates its output directory: spliced above it,
	// the redirect would fail on the machine's main process.
	if mkdirAt := strings.Index(p.Entrypoint, "mkdir -p /newroot"); mkdirAt < 0 || mkdirAt > spliceAt {
		t.Errorf("the capture is spliced before the mkdir that creates /newroot/etc/devboxes —\n"+
			"its redirect would fail during boot (mkdir at %d, capture at %d)", mkdirAt, spliceAt)
	}
	// …and before the pivot, after which /newroot no longer exists and the
	// redirect fails into `|| :` — a silent no-capture.
	if pivotAt := strings.Index(p.Entrypoint, "pivot_root"); pivotAt < 0 || pivotAt < spliceAt {
		t.Errorf("the capture is spliced after pivot_root — /newroot is gone by then, so the\n"+
			"write fails silently (pivot_root at %d, capture at %d)", pivotAt, spliceAt)
	}

	const (
		seeded   = "# Generated by Fly\nnameserver fdaa::3\nsearch example.internal\noptions edns0\n"
		lastGood = "nameserver fdaa::9\n"
	)
	cases := []struct {
		name      string
		resolv    string             // "" = no /etc/resolv.conf at all
		mkSrc     func(string) error // overrides resolv: create something that is not a regular file
		prior     string             // pre-existing capture on the persisted volume
		priorMode os.FileMode        // 0 = 0o644; use 0o444 to make the write itself fail
		want      string
	}{
		// The first case is the real thing: a staging probe (2026-08-05) shows
		// Fly seeds exactly `nameserver\tfdaa::3` — TAB-separated, no search, no
		// options — so a matcher that assumed a space would capture nothing.
		{"the shape Fly actually seeds", "nameserver\tfdaa::3\n", nil, lastGood, 0, "nameserver\tfdaa::3\n"},
		{"a seed with search and options is captured verbatim", seeded, nil, "", 0, seeded},
		{"no nameserver keeps last-known-good", "# Generated by resolvconf\noptions edns0\n", nil, lastGood, 0, lastGood},
		{"no resolv.conf at all keeps last-known-good", "", nil, lastGood, 0, lastGood},
		{"final line with no trailing newline is captured", "nameserver fdaa::3", nil, "", 0, "nameserver fdaa::3\n"},
		{"an indented line is not a nameserver", "  nameserver fdaa::3\n", nil, lastGood, 0, lastGood},
		// The two shapes that would kill the machine rather than skip a boot:
		// errexit is armed inside the `if`, so a directory aborts the entrypoint
		// without `|| :`, and a FIFO blocks forever without the `-f` guard.
		// Each proves one half independently.
		{"a directory does not abort the entrypoint", "", func(p string) error { return os.Mkdir(p, 0o755) }, lastGood, 0, lastGood},
		{"a FIFO does not hang the entrypoint", "", mkFifo, lastGood, 0, lastGood},
		// The `|| :` half: a write that fails must not take the machine down.
		{"an unwritable capture target does not abort the entrypoint", "nameserver fdaa::3\n", nil, lastGood, 0o444, lastGood},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src, dst := filepath.Join(dir, "resolv.conf"), filepath.Join(dir, "boot-resolv.conf")
			switch {
			case tc.mkSrc != nil:
				if err := tc.mkSrc(src); err != nil {
					t.Fatalf("create fixture at %s: %v", src, err)
				}
			case tc.resolv != "":
				if err := os.WriteFile(src, []byte(tc.resolv), 0o644); err != nil {
					t.Fatalf("write fixture resolv.conf: %v", err)
				}
			}
			if tc.prior != "" {
				mode := tc.priorMode
				if mode == 0 {
					mode = 0o644
				}
				if err := os.WriteFile(dst, []byte(tc.prior), mode); err != nil {
					t.Fatalf("write prior capture: %v", err)
				}
			}
			script := strings.NewReplacer(
				"/etc/resolv.conf", src,
				"/newroot"+capturePath, dst,
			).Replace(p.Capture)

			// -eux is what the entrypoint runs with. PATH must be present and
			// EMPTY, not absent: bash substitutes a compiled-in default PATH
			// when the variable is unset, which on a distro runner resolves a
			// full /usr/bin userland and makes this guard vacuous.
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-euxc", script)
			cmd.Env = []string{"PATH="}
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("the capture never returned — in the entrypoint that is a machine that never\n"+
					"finishes booting\n--- trace ---\n%s", out)
			}
			if err != nil {
				t.Fatalf("the capture aborted (exit %v) — under `set -eux` in the entrypoint that is a\n"+
					"dead machine, not a missing nameserver\n--- trace ---\n%s", err, out)
			}
			got, err := os.ReadFile(dst)
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("read capture: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("capture = %q, want %q", got, tc.want)
			}
		})
	}

	// The unit that consumes the capture. Registers (`-a`) and reads the capture
	// from STDIN — openresolv takes the record on stdin, so a positional file
	// would be a usage error and the unit would fail. Independent matches rather
	// than one, so reordering or adding flags is not a false failure.
	registers := regexp.MustCompile(`/bin/resolvconf\b[^\n]*\s-a\s`).MatchString(p.ResolvScript)
	fromCapture := regexp.MustCompile(`<\s*` + regexp.QuoteMeta(capturePath) + `\b`).MatchString(p.ResolvScript)
	if !registers || !fromCapture {
		t.Errorf("devboxes-resolv no longer registers the captured nameservers with\n"+
			"`resolvconf -a … < /etc/devboxes/boot-resolv.conf` — its edges are intact but the\n"+
			"payload is gone\n--- script ---\n%s", p.ResolvScript)
	}
	// The metric is what keeps this a fallback: without it openresolv's default
	// ranks the captured seed ahead of both stage-2's host record and a client
	// image's own networking.nameservers, silently overriding a resolver the
	// client configured.
	if !directiveHasToken(p.ResolvUnit, "ConditionFileNotEmpty", capturePath) {
		t.Errorf("devboxes-resolv no longer gates on ConditionFileNotEmpty=%s — point it at a path\n"+
			"nothing writes and the unit skips on every boot forever, silently undoing the fix\n--- resolv ---\n%s",
			capturePath, p.ResolvUnit)
	}
	// The metric and the non-`lo` key are one property: openresolv hoists `lo.*`
	// ahead of the metric ordering, so an `lo.` key defeats `-m 1100` entirely.
	if key := regexp.MustCompile(`\s-a\s+(\S+)`).FindStringSubmatch(p.ResolvScript); key != nil &&
		regexp.MustCompile(`^lo[0-9]*([.:]|$)`).MatchString(key[1]) {
		t.Errorf("devboxes-resolv registers under %q — openresolv's key_order globs `lo.*` to the head\n"+
			"of the list before metrics are read, so the captured seed would override both the host\n"+
			"record and a client image's own nameservers", key[1])
	}
	if !regexp.MustCompile(`\s-m\s+1100\b`).MatchString(p.ResolvScript) {
		t.Errorf("devboxes-resolv lost its `-m 1100` metric — the boot-captured record stops\n"+
			"being a fallback and starts overriding better resolvers\n--- script ---\n%s", p.ResolvScript)
	}
	// openresolv's libc subscriber exits 1 for any command but `-u` when
	// /etc/resolv.conf lacks its signature, so this edge is what keeps the unit
	// from failing outright on a first boot.
	if !directiveHasToken(p.ResolvUnit, "After", "resolvconf.service") {
		t.Errorf("devboxes-resolv is no longer ordered after resolvconf.service — `resolvconf -a`\n"+
			"can then run against an un-normalized /etc/resolv.conf and fail\n--- resolv ---\n%s", p.ResolvUnit)
	}

	// Ordering asserted as a property: moving the edge to the agent's After= is
	// identical to systemd, so it must not be a false failure.
	orderedBefore := directiveHasToken(p.ResolvUnit, "Before", "devboxes-agent.service") ||
		directiveHasToken(p.AgentUnit, "After", "devboxes-resolv.service")
	if !orderedBefore {
		t.Errorf("nothing orders devboxes-resolv before devboxes-agent — the agent can start\n"+
			"against a resolver that has no nameserver and burn its backoff\n--- resolv ---\n%s\n--- agent ---\n%s",
			p.ResolvUnit, p.AgentUnit)
	}
	pulledIn := directiveHasToken(p.AgentUnit, "Wants", "devboxes-resolv.service") ||
		directiveHasToken(p.AgentUnit, "Requires", "devboxes-resolv.service")
	if !pulledIn {
		t.Errorf("devboxes-agent no longer pulls in devboxes-resolv — the re-assert never runs\n"+
			"for the unit that depends on it\n--- agent ---\n%s", p.AgentUnit)
	}
}
