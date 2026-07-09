# Rift environment scaffolding playbook

You are a coding agent. Your job is to add a **Rift environment** to the repository
in the current working directory by producing a `fixed-labs.rift` output in
`./flake.nix`. This output declares the Nix image a Rift box boots for this repo.

Follow the six steps below **in order**. Do not skip the validate step.

You have `nix` on `PATH`. If a `nix` command reports that `nix` is not installed,
stop and tell the user Rift scaffolding requires Nix.

---

## 1. Audit

Read the repository's declared toolchain and dependencies. Inspect (whichever
exist):

- `package.json`, `package-lock.json`, `pnpm-lock.yaml`, `yarn.lock` — Node.js
  runtime + versions; note npm/pnpm/yarn.
- `go.mod`, `go.sum` — Go toolchain version.
- Other `*.lock` files (`Cargo.lock`, `poetry.lock`, `Gemfile.lock`, `flake.lock`, …)
  — language + package managers in use.
- `.tool-versions` (asdf/mise) — pinned runtimes.
- `Dockerfile` / `docker-compose.yml` — system packages (`apt-get install …`),
  services (databases, caches), and base images.
- CI configs (`.github/workflows/*.yml`, `.gitlab-ci.yml`, `.circleci/config.yml`)
  — the tools CI installs before building/testing.
- `Makefile` — invoked binaries and build steps.
- `README` — documented prerequisites and setup instructions.

From these, infer the full set of needs:

- **Language runtimes** (e.g. Node.js, Go, Python, Ruby, Rust).
- **System libraries** (e.g. OpenSSL, zlib, libpq, a C toolchain) — often implied by
  native addons or `apt-get` lines, not just top-level language deps.
- **CLI tools** (e.g. `jq`, `ripgrep`, `awscli`, a database client).
- **Services** the code expects to reach (Postgres, Redis, …). Note them for the
  user; Rift boxes run a single NixOS environment, so a service is typically the
  server package plus the client.

## 2. Map

Map each need to a **nixpkgs attribute name**. Do **not** guess attribute names —
confirm each one:

```
nix search nixpkgs <term>
```

Pick the exact attribute (e.g. Node.js 20 → `nodejs_20`, Go → `go`, Python 3 →
`python3`, the Postgres client → `postgresql`, ripgrep → `ripgrep`). For packages
that live under a set, use the dotted attribute path exactly as nixpkgs exposes it
(e.g. `python3Packages.numpy`).

Collect the confirmed attribute names into a single comma-separated list.

## 3. Emit

Run the emit command with your package list. Add `--option k=v` for any
base-module option you need to set (repeatable); omit `--option` entirely if you
need none:

```
rift init emit --packages <attr1>,<attr2>,<attr3> [--option k=v ...]
```

`emit` prints exactly two comment-delimited pieces to stdout:

1. an **inputs** piece — a single `inputs.rift.url = …;` line.
2. an **outputs** piece — a complete `fixed-labs.rift = rift.lib.mkRift { … };`
   block whose `extraModules` sets `environment.systemPackages` (and one
   `rift.devboxes-base.<k> = <v>;` line per `--option`).

`emit` is pure string substitution — it validates nothing. A malformed result is
caught by the eval in step 5, not here.

Example — a Go repo that also wants the login user set:

```
rift init emit --packages go --option loginUser='"dev"'
```

## 4. Splice

Put emit's two pieces into `./flake.nix`.

**If a `flake.nix` already exists**, merge:

- Add the **inputs** piece into the `inputs` set. If an `inputs.rift` already exists
  under `rift` (or any other name), **replace** it — do not append a duplicate
  attribute (a duplicate is an eval error). The emitted pieces always name the input
  `rift`, so if the flake bound it under another name, rewrite the references to
  `rift`.
- Add the **outputs** piece into the `outputs` body. If a `fixed-labs.rift` already
  exists, **replace** it rather than appending.
- Ensure the `outputs = { … }:` argument set binds **both** `self` and `rift`. The
  emitted pieces reference both; `outputs = { self, nixpkgs }: …` fails with
  `undefined variable 'rift'`. Add `rift` (and `self`) to the arg set if missing.

**If no `flake.nix` exists**, create one from this skeleton and drop emit's two
pieces into the marked slots. **No `nixpkgs` input is needed** — the module's `pkgs`
comes from `mkDevimage`'s pinned nixpkgs via the `rift` input:

```nix
{
  # ← emit's INPUTS piece (inputs.rift.url = …;)
  outputs = { self, rift, ... }: {
    # ← emit's OUTPUTS piece (fixed-labs.rift = rift.lib.mkRift { … };)
  };
}
```

## 5. Validate

Evaluate the contract (this stops before building — eval only):

```
nix eval .#fixed-labs.rift.image.drvPath
```

If it errors, read the error, fix the offending finding in your fragment, and
re-eval. **Loop until it evaluates.** Common fixes:

- A bad package attribute → correct it (re-check with `nix search nixpkgs <term>`).
- An unbound variable like `rift` or `self` → fix the `outputs = { … }:` arg set.
- A malformed `--option` value (`<v>` is a raw Nix expression) → re-emit with valid
  Nix (e.g. a string needs quotes: `--option loginUser='"dev"'`, not `loginUser=dev`).

This eval exercises the **full** module system + nixpkgs, so a failure is
occasionally unrelated to your chosen packages (a base-module or nixpkgs eval
error). Scope your fixes to your own fragment.

## 6. Report

Show the user:

- the resulting `./flake.nix` (or the diff you applied), and
- the packages you chose, with a one-line rationale each (which audited signal led
  to it).
