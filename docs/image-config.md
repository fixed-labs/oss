# Customizing the base image

Every Rift devbox boots from an OCI image built on a required **base module**,
`nixosModules.devboxes-base`. The base module provides the substrate the service
depends on — the overlay-root boot, the WireGuard interface, and the in-box agent
that `rift connect` talks to. You layer your own toolchain, editor, and dotfiles
on top and bake your image with `lib.mkDevimage`.

You do **not** run a CI pipeline to build your image. When you push, Rift's
managed builder clones your repository and runs a **pure** `nix build .#devimage`
against your flake. Your job is to define that `.#devimage` output.

## The client flake

A minimal custom-image flake takes this repository as an input, imports the base
module, and calls `mkDevimage`:

```nix
{
  inputs.rift.url = "github:fixed-labs/oss";

  outputs = { self, rift, ... }: {
    packages.x86_64-linux.devimage = rift.lib.mkDevimage {
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

      # Bake your repo's own source into the image (see below).
      repoSrc = self;
      imageCommit = self.rev or null;
    };
  };
}
```

See [`examples/flake.nix`](../examples/flake.nix) for a complete, copyable version.

## `mkDevimage` parameters

| Parameter | Purpose |
|---|---|
| `extraModules` | NixOS modules layered on top of the base. This is where your tools, packages, services, and base-module option settings go. `mkDevimage` already bakes in `nixosModules.devboxes-base`, so you need not add it yourself. |
| `repoSrc` | The source tree to bake into the image (see next section). `null` bakes nothing. |
| `imageCommit` | A commit identifier stamped into the image, for traceability. |
| `repoDirName` | The directory name your repo is baked to inside the box (default `"repo"`). |
| `name` / `tag` | The image name and tag (default `"devimage"` / `"latest"`). |
| `hostSystem` | The build platform (default `x86_64-linux`) — where the build runs. The image *content* is always x86_64; this only selects the builder. |

## Configuring the base

The base module exposes options under the `rift.devboxes-base` namespace — for
example `rift.devboxes-base.loginUser` (the single user every connection lands as)
and `rift.devboxes-base.loginShell`. Set them from any module in your
`extraModules` list. The base module wires the in-box agent automatically; you do
not supply it.

## Baking your repo — use `repoSrc = self`

To have your repository's code present inside the box at boot, bake it in with
`repoSrc`. Because the managed build is a **pure** `nix build` (no `--impure`, no
environment variables), you must bake from the flake's own source:

```nix
repoSrc     = self;             # the cloned checkout the build runs against
imageCommit = self.rev or null; # the commit, when the tree is a clean git checkout
```

Do **not** reach for an impure pattern like `builtins.getEnv` to locate the source
— under the pure managed build it reads as empty and silently bakes **nothing**.
`self` is the correct, pure handle to the checked-out tree.

One caveat: a flake's `self` omits the `.git` directory. Baking with `repoSrc =
self` therefore lands your working tree in the box but **not** its git history, so
in-box `git pull` is not wired up by baking alone. Getting a fully clonable,
pull-able repo into the box is a follow-up beyond plain baking.

## Exposing the output

Managed builds build the `packages.x86_64-linux.devimage` attribute, so expose
exactly that name:

```nix
packages.x86_64-linux.devimage = rift.lib.mkDevimage { /* … */ };
```

That single output is the whole image contract.
