# Getting started

This walks you from nothing to a shell inside a running Rift devbox: install the
`rift` CLI, log in, spawn a box, and connect.

## 1. Install the `rift` CLI

The recommended way to install `rift` is with [Homebrew](https://brew.sh):

```sh
brew install fixed-labs/tap/rift
```

Once you have tapped it with `brew tap fixed-labs/tap`, a bare `brew install rift`
also works, and `brew upgrade rift` updates it later. (The `tap` shorthand resolves
to the `fixed-labs/homebrew-tap` repo — Homebrew strips the `homebrew-` prefix.)

Or build it from source. The CLI lives in the `cli/` Go module:

```sh
cd cli
go build ./cmd/rift        # produces ./rift in the current directory
```

Or install it onto your `PATH` with the Go toolchain:

```sh
go install github.com/fixed-labs/oss/cli/cmd/rift@latest
```

Or build it with Nix from the repository root:

```sh
nix build .#rift           # result at ./result/bin/rift
```

Put the resulting `rift` binary somewhere on your `PATH`.

## 2. Log in

Rift authenticates with a **device flow** — no password is typed into the
terminal:

```sh
rift login
```

`rift login` prints a verification URL and a short user code, then waits. Open the
URL in a browser that is already signed in to Rift, enter the code, and approve.
The moment you approve, the CLI receives a bearer token and stores your session at
`~/.config/rift/config.json`. Every later command reads that token from the config
file; you only log in again when it expires.

The control plane the CLI talks to is not compiled in — it comes from your login
config and the `RIFT_*` environment. The default points at the hosted service.

## 3. Spawn a devbox

From inside a git repository you want to work on:

```sh
rift new
```

`rift new` creates a workspace (a devbox) for the current repository, inferring
the repo from the `origin` git remote. The service picks a base image built for
your repo, boots a VM, and returns a workspace id. Useful flags:

- `rift new --repo <repo>` — name the repo explicitly instead of inferring it.
  Accepts `owner/name` (assumed to live on github.com), a clone URL
  (`https://…` or `git@…`), or the full canonical form
  `github:github.com/owner/name`. Only GitHub.com repositories are supported
  today; `--forge` declares the forge type for a host the service doesn't
  recognize (only `github` exists so far).
- `rift new --size <id>` — choose a VM size (see `rift new --help`; the offered
  sizes come from the service).
- `rift new --region <fly-region>` — pick a compute region.
- `rift new --ref <branch>` (e.g. `--ref main`) or `--image <commit-sha>` — override
  which built image the box boots from (mutually exclusive).
- `rift new --context <ctx>` — scope the workspace to a named context (e.g. a
  personal vs. an organization account). `rift ls --context <ctx>` filters the
  list to it, and `rift set-default-context <ctx>` picks the default used when
  `--context` is omitted.

The box takes a short while to reach the running state; `rift connect` (next step)
waits for it.

## 4. Connect

```sh
rift connect
```

`rift connect` opens an interactive shell in your devbox. Behind the scenes:

1. The CLI generates an ephemeral **WireGuard** keypair and asks the service to
   attach your laptop to the box. The service returns the box's WireGuard public
   key, the tunnel endpoint (a relay), and the box's SSH host key.
2. The CLI brings up a WireGuard tunnel to the box. All further traffic —
   including the SSH session — flows encrypted inside that tunnel; nothing is
   exposed on the public internet.
3. Over the tunnel, the CLI speaks **SSH to the in-box agent** (`devboxes-agent`,
   baked into the image from this repo's `agent/` module). The agent authorizes
   your tunnel peer and drops you into a shell as the box's login user.

While the session is live the CLI sends periodic presence pings so the box is not
idle-suspended out from under you. Close the shell to disconnect; the box keeps
your data and suspends after its idle window, ready to resume next time.

## Everyday commands

```sh
rift ls                    # list your devboxes
rift connect <id>          # connect to a specific box
rift rm <id>               # destroy a box
```

Run `rift --help` (or `rift <command> --help`) for the full set.

## Where to go next

- [secrets.md](secrets.md) — get credentials (AWS, npm, GitHub, SSH agent, …)
  into your box safely.
- [image-config.md](image-config.md) — customize the base image with your own
  tools and dotfiles.
- [api-reference.md](api-reference.md) — the HTTP endpoints the CLI calls, if you
  are integrating directly.
