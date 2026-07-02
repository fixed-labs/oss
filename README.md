# Rift

Rift is a hosted dev-environment service. You launch a **devbox** — a
single-tenant, persistent Linux developer machine in the cloud — and connect to
it from your laptop as if it were local. Each box boots from a base image you can
customize, keeps your data across restarts, and idle-suspends when you step away.

This repository is the **open client surface** of that service: everything you
need to build against, connect to, and customize a Rift devbox. It contains:

- **`cli/`** — the `rift` command-line tool: `rift login`, `rift new`,
  `rift connect`, and the rest of the workspace lifecycle.
- **`agent/`** — the in-box agent (`devboxes-agent`) that the base image bakes.
  It runs inside every devbox and serves the SSH endpoint `rift connect` speaks
  to over the tunnel.
- **`nix/devboxes-base/`** — the NixOS base module every devbox image builds on.
  Your custom image imports it and layers your tools on top.
- **`examples/`** — a sample client flake (`flake.nix`) and a sample secrets
  manifest (`secrets.json`) to copy from.
- **`docs/`** — integration guides (see below).

The control plane, the relay data plane, and the service's internal libraries
are **not** part of this repository — the pieces here bind to a Rift deployment
only through `rift login` and `RIFT_*` environment configuration, so the same
binaries work against the hosted service or any other deployment.

## Quickstart

See **[docs/getting-started.md](docs/getting-started.md)** for the full path from
install to a running shell. In brief:

```sh
cd cli && go build ./cmd/rift    # or: nix build .#rift
./rift login                     # device-flow login in your browser
./rift new                       # spawn a devbox from your repo
./rift connect                   # open a shell in it over WireGuard + SSH
```

## Repository layout — two Go modules, no root module

There is **no top-level `go.mod`**. `cli/` and `agent/` are two independent Go
modules with no cross-imports, so build commands run **inside** a module:

```sh
# The rift CLI
cd cli && go build ./cmd/rift
go install github.com/fixed-labs/oss/cli/cmd/rift@latest
nix build .#rift

# The in-box agent
cd agent && go build ./cmd/devboxes-agent
nix build .#agent
```

## Documentation

| Guide | What it covers |
|---|---|
| [docs/getting-started.md](docs/getting-started.md) | Install → `login` → `new` → `connect`, and the connection model. |
| [docs/secrets.md](docs/secrets.md) | The `.rift/secrets.json` manifest, your `~/.config/rift/secrets.json`, and the `std:`/`local:`/`org:` key model. |
| [docs/image-config.md](docs/image-config.md) | Customizing the base image: `nixosModules.devboxes-base` + `lib.mkDevimage`. |
| [docs/api-reference.md](docs/api-reference.md) | The developer-surface HTTP endpoints the CLI calls. |

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
