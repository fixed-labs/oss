# Customizing the base image

Every Rift devbox boots from an OCI image built on a required **base module**,
`nixosModules.devboxes-base`. The base module provides the substrate the service
depends on — the overlay-root boot, the WireGuard interface, and the in-box agent
that `rift connect` talks to. You layer your own toolchain, editor, and dotfiles
on top and declare your image through the `fixed-labs.rift` contract with
`lib.mkRift`.

You do **not** run a CI pipeline to build your image. When you push, Rift's
managed builder clones your repository, reads the versioned `fixed-labs.rift`
contract from your flake, and builds `#fixed-labs.rift.image` against the
FPL-pinned base. Your job is to declare that `fixed-labs.rift` output.

## The contract output

The environment is declared at the custom top-level flake output
`fixed-labs.rift` — **not** under `packages`. It is a versioned, self-describing
envelope:

```nix
fixed-labs.rift = {
  version = 1;                          # integer; the builder dispatches on this
  image   = <x86_64-linux derivation>;  # the devbox image, layered on devboxes-base
};
```

Keeping it out of `packages` means a bare `nix build` or `nix flake check` no
longer tries to build your heavy image — `packages` stays yours. `fixed-labs.rift`
is a **fixed literal path**: the leading `fixed-labs` echoes the vendor org but is
a hardcoded constant the builder matches on. It is independent of what you name the
`rift` flake input — bind the input as `myrift` if you like and call
`myrift.lib.mkRift`, but the *output* must still be literally `fixed-labs.rift`.

You never write `version` or hand-build the envelope: `lib.mkRift` produces it.

## The client flake

A minimal custom-image flake takes this repository as an input and calls
`mkRift`, passing `self`:

```nix
{
  inputs.rift.url = "github:fixed-labs/oss";

  outputs = { self, rift, ... }: {
    fixed-labs.rift = rift.lib.mkRift {
      inherit self;

      # Your NixOS modules — tools, editors, dotfiles, services.
      extraModules = [
        # Optional: mkDevimage already bakes in devboxes-base. Listed here only
        # to make the layering explicit (the module system dedupes it).
        rift.nixosModules.devboxes-base
        ({ pkgs, ... }: {
          environment.systemPackages = [ pkgs.go pkgs.ripgrep pkgs.jq ];
          # Configure the base via its options if you like:
          # rift.devboxes-base.loginUser = "dev";
        })
      ];
    };
  };
}
```

See [`examples/flake.nix`](../examples/flake.nix) for a complete, copyable version.

## `mkRift` and `mkDevimage`

`rift.lib.mkRift` is the helper you call. It wraps the low-level primitive
`rift.lib.mkDevimage` — the actual image packer — and does two extra jobs:

- Produces the versioned `{ version = 1; image = … }` envelope, so you never write
  the version.
- Defaults `repoSrc = self` and `imageCommit = self.rev or null`, so your
  checkout is baked in without restating it (see *Baking your repo* below).

Every argument you pass to `mkRift` other than `self` is forwarded to
`mkDevimage`. `mkDevimage`'s argument pattern is **closed**, so a typo'd or
unknown argument fails the build with Nix's `called with unexpected argument`
error — that delegation *is* the validation. Its parameters:

| Parameter | Purpose |
|---|---|
| `extraModules` | NixOS modules layered on top of the base. This is where your tools, packages, services, and base-module option settings go. `mkDevimage` already bakes in `nixosModules.devboxes-base`, so you need not add it yourself. |
| `repoSrc` | The source tree to bake into the image (see next section). Defaulted to `self` by `mkRift`; `null` bakes nothing. |
| `imageCommit` | A commit identifier stamped into the image, for traceability. Defaulted to `self.rev or null` by `mkRift`. |
| `repoDirName` | The directory name your repo is baked to inside the box (default `"repo"`). |
| `name` / `tag` | The image name and tag (default `"devimage"` / `"latest"`). |
| `hostSystem` | The build platform (default `x86_64-linux`). Builder-controlled — always `x86_64-linux` under a managed build — so you must **not** set it. |

Base-module options (`rift.devboxes-base.*`, e.g. `loginUser`) are **not**
`mkDevimage` arguments — set them as NixOS options inside an `extraModules` entry,
as the flake above shows.

## Configuring the base

The base module exposes options under the `rift.devboxes-base` namespace — for
example `rift.devboxes-base.loginUser` (the single user every connection lands as)
and `rift.devboxes-base.loginShell`. Set them from any module in your
`extraModules` list. The base module wires the in-box agent automatically; you do
not supply it.

## Baking your repo

To have your repository's code present inside the box at boot, `mkRift` bakes it
automatically: it defaults `repoSrc = self`, so under the pure managed build
(`self` is the checked-out tree) your source is present in the box with no extra
configuration. There is no impure `builtins.getEnv` pattern to reach for — under
the pure build that reads as empty and would silently bake **nothing**; `self` is
the correct, pure handle.

One caveat: a flake's `self` omits the `.git` directory. The default
`repoSrc = self` therefore lands your working tree in the box but **not** its git
history, so in-box `git pull` is not wired up by baking alone. A consumer that
needs `.git` overrides `repoSrc` with a clone that carries it. The two defaults
are independent: overriding `repoSrc` **without** also setting `imageCommit`
stamps `self`'s rev, not the overridden clone's — **override both** for correct
provenance:

```nix
fixed-labs.rift = rift.lib.mkRift {
  inherit self;
  repoSrc     = <a clone with .git>;
  imageCommit = <that clone's commit>;   # override both, or provenance is wrong
  extraModules = [ /* … */ ];
};
```

## Validating locally

The managed builder builds `#fixed-labs.rift.image`. You cannot (and need not)
build it yourself, but you can check that your flake evaluates — exercising the
full module system + nixpkgs, stopping before realisation — with an eval-only
command that any architecture can run:

```sh
nix eval .#fixed-labs.rift.image.drvPath
```

If this succeeds, your package names and module structure are valid Nix; the
managed builder will do the actual build. A failure here is your fragment to fix
(occasionally a base-module or nixpkgs eval error surfaces too — scope fixes to
your own additions).
