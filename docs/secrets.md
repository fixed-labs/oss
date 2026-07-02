# Secrets

Rift can push credentials from your laptop into a devbox — AWS keys, an npm token,
a GitHub token, your SSH agent, and so on — without those secrets ever touching
the control plane. Two files drive it, split by trust:

- **`.rift/secrets.json`** — the **repo manifest**, checked into your repository.
  It declares *which* secrets a box wants and *where* they land. It is untrusted:
  it may name a destination, but it may **never** name a source. A malicious or
  mistaken manifest can therefore ask for a secret, but it can never point that
  request at a file on your machine.
- **`~/.config/rift/secrets.json`** — your **user config**, private to your
  laptop. It supplies the *sources* — the actual files or commands that produce
  the secret bytes. It lives right next to your login config (`config.json`) in
  the same `~/.config/rift/` directory.

A secret is only ever pushed when the manifest declares it *and* your user config
maps it to a source. The repo asks; you decide with what.

## The key model: `std:`, `local:`, `org:`

Every secret is named by a namespaced key, `namespace:name`. The namespace says
who owns the key's meaning:

| Namespace | Meaning |
|---|---|
| `std:`   | **Tool-defined built-ins.** Rift knows the destination, file mode, and delivery strategy for these — you only supply the source. Examples: `std:aws`, `std:gcp`, `std:npm`, `std:netrc`, `std:github`, `std:claude`, `std:ssh`, `std:gpg`, `std:ssh-key`. |
| `local:` | **This-repo keys.** The manifest declares the destination (or an environment-variable name) itself; you map the source in your user config. Use these for credentials specific to one project. |
| `org:`   | **Reserved** for organization-wide keys. Not yet available. |

A bare name with no namespace (e.g. `aws`) is treated as `local:` — write `std:aws`
when you mean the built-in.

## The repo manifest — `.rift/secrets.json`

The manifest is a list of declared secrets:

```json
{
  "secrets": [
    { "key": "std:aws" },
    { "key": "std:github" },
    { "key": "local:service-token", "env": "SERVICE_TOKEN" }
  ]
}
```

Fields per entry:

- **`key`** (required) — the namespaced key.
- **`dest`** — a home-relative destination path (must start with `~/`), for a
  `local:` secret delivered as a file. Ignored for `std:` keys (Rift owns their
  destination) and mutually exclusive with `env`.
- **`env`** — an environment-variable name, for a `local:` secret delivered as an
  env value instead of a file. Mutually exclusive with `dest`.
- **`mode`** — octal file mode for a `dest` secret (default `0600`; group/other
  are limited to read-only).
- **`tmpfs`** — whether the pushed file is tmpfs-backed so it evaporates when the
  box stops (default `true`).
- **`description`** — a human-readable note shown in prompts.

Only destinations appear here. Sources never do.

See [`examples/secrets.json`](../examples/secrets.json) for a copyable starting
point.

## The user config — `~/.config/rift/secrets.json`

Your user config maps keys to **sources** and records per-repo push policy:

```json
{
  "defaults": {
    "aws": "~/.aws/credentials",
    "npm": { "cmd": "cat ~/.npm-token" }
  },
  "repos": {
    "your-org/your-repo": {
      "policy": "ask",
      "map": {
        "local:service-token": "~/secrets/service-token"
      }
    }
  }
}
```

- **`defaults`** — sources for `std:` keys, shared across every repo, keyed by the
  bare name (`aws`, `npm`, …). This is what makes `{"key":"std:aws"}` in a manifest
  work with zero per-repo setup.
- **`repos.<owner/name>.map`** — per-repo source mappings, keyed by the full
  namespaced key (`local:service-token`). A per-repo mapping overrides a default.
- **`repos.<owner/name>.policy`** — how a push is authorized: `ask` (default —
  prompt before pushing), `auto-push`, or `off`.

### Source forms

A source is one of:

- **A file path** — `"~/.aws/credentials"` (with `~/` expansion). Its bytes are the
  secret.
- **A command** — `{ "cmd": "some-command" }`. The command's stdout is the secret.
  Only *your* user config can supply a command; the repo never can, so running it
  is safe.
- **`"forward"`** — the literal string, for agent-forwarded secrets (`std:ssh`,
  `std:gpg`). No bytes are copied to the box; your local agent is forwarded over the
  connection instead.

Because sources live only in your user config and the file is written with `0600`
permissions, the repository you are working on never learns where your credentials
come from — only that it asked for them.
