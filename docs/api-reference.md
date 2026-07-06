# HTTP API reference (developer surface)

This is the HTTP surface the `rift` CLI calls on your behalf — the endpoints a
developer integrating directly against a Rift deployment needs. It is documented
at the level of endpoint, method, and purpose.

There is no OpenAPI or IDL for this API, and we do not generate one. The
authoritative description of every request and response body is the CLI's own Go
client package, **`cli/internal/client`** — each endpoint below maps to a method
and a JSON-tagged struct there. When a field is load-bearing, read the struct.

## Authentication

Every endpoint except the device-login pair takes a **bearer token** obtained from
`rift login` (stored in `~/.config/rift/config.json`):

```
Authorization: Bearer <token>
```

The base URL of the deployment is not compiled in; it comes from your login config
and `RIFT_*` environment. Non-2xx responses carry a status and a body; the CLI
branches on status (e.g. `409` image-not-ready, `503` no-ready-relay/pool-full,
`304` long-poll-hold-timeout).

## Workspaces

A *workspace* is a devbox. `Workspace` / `ListItem` are the read shapes;
`CreateResult` is the create echo.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/workspaces` | Create a workspace. Body carries `repo` (the canonical repo id — see [Repo ids](#repo-ids)), `context_id`, `fallback_to_default`, and optional `size`, `region`, `ref`, or `image` (`ref`/`image` are mutually exclusive boot-selection overrides). Returns the new `workspace_id` and the resolved ref/commit. |
| `GET`  | `/api/workspaces` | List your workspaces (a single snapshot read). Returns `{ "workspaces": [ … ] }`. |
| `GET`  | `/api/workspaces/:id` | Read one workspace. A bare read is a snapshot; passing `?cursor=<c>` turns it into a **long poll** that holds until the workspace changes and returns a new cursor — the primitive `rift connect` watches with. A hold-timeout answers `304` (no change; re-poll with the same cursor). |
| `DELETE` | `/api/workspaces/:id` | Destroy a workspace. |

### Lifecycle subroutes

All are `POST /api/workspaces/:id/<action>`:

| Action | Body | Purpose |
|---|---|---|
| `suspend` | — | Suspend (stop) the box; data persists. |
| `resume`  | — | Resume a suspended box. |
| `resize`  | `{ "size": "<id>" }` | Change the box's VM size. |
| `keepalive` | `{ "for_ms": <int> }` | Hold the box awake for a window (defer idle-suspend). |
| `presence` | — | Liveness ping the running `connect` sends on a short cadence so an active session is not idle-suspended. |
| `attach`  | `{ "laptop_wg_pubkey": "<key>", "login_user": "<user>" }` | Open an attachment for your laptop's WireGuard public key. Returns the transport bundle — the relay endpoint/port, the box's WireGuard public key and IP, your assigned laptop IP, and the box's SSH host key — everything `connect` needs to bring up the tunnel. `login_user` is optional. |
| `detach`  | `{ "laptop_wg_pubkey": "<key>" }` | Close a previously opened attachment. |

## VM sizes

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/workspaces/sizes` | The offered VM sizes plus the effective default a blank `rift new` resolves to. Returns `{ "effective_default": <id-or-null>, "sizes": [ … ] }`; each size carries `id`, `display_name`, `description`, `cpu`, `memory_mb`, and a display-only `price` string. |

## Contexts

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/contexts` | The contexts the caller may act in (e.g. a personal vs. an organization account). Returns `{ "contexts": [ { "form_value": "<stable-id>", "label": "<human label>" } ] }`. The `form_value` is what `rift new --context`/`rift ls --context`/`rift set-default-context` pass. |

## Repo ids

Everywhere this API carries a repository, it carries the **canonical
forge-qualified repo id**: `forge:host/owner/name`, all lowercase — e.g.
`github:github.com/acme/widget`. Only forge `github` on host `github.com` is
accepted today; the server validates the id at ingress and rejects anything
non-canonical with a `400`. The `rift` CLI derives this id for you from a git
remote, an `owner/name` pair, or a clone URL.

## Images

The base images a repo's boxes can boot from, keyed by commit.

| Method | Path | Purpose |
|---|---|---|
| `GET`  | `/api/repos/images?repo=<repo>` | List the live (bootable) base images for a repo, newest-first. Optional `&limit=<n>`. Each entry carries the commit, creation time, registry ref, pin state, box count, the refs the commit heads, and whether it is the default-branch head. |
| `POST` | `/api/repos/images/:commit/pin?repo=<repo>` | Pin a `(repo, commit)` image so it is never reaped. |
| `POST` | `/api/repos/images/:commit/unpin?repo=<repo>` | Clear the pin. |

`repo` is the canonical repo id (see [Repo ids](#repo-ids)), percent-encoded as
a whole. It rides as a **query parameter**, not path segments, because the
canonical id contains `:` and `/`.

## Device-flow login

The one endpoint pair that does **not** take a bearer — it mints one.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/login/device/start` | Begin a login. Returns `{ device_code, user_code, verification_url, interval }`. The CLI prints the URL and user code for the human to approve in a browser. |
| `POST` | `/api/login/device/poll` | Poll for approval, body `{ "device_code": "<code>" }`. This is a server-side **long poll**: `200 { token, context }` the moment the human approves, `204` (also `202`) on a hold-timeout while still pending (re-poll), or a terminal `4xx` (e.g. expired/denied). |

## Not covered here

Three surfaces exist on a Rift deployment but are **out of scope** for this
client reference:

- **The in-VM machine-token self-service surface** (`/api/rift/v1/…`). These
  routes are called by `rift` running *inside* a box, authenticated by the box's
  own machine token (a workspace acting on itself) — not integrated by external
  developer clients.
- **The relay surface** — the data plane the tunnel rides on.
- **The admin surface** — deployment operator tooling.
