# Minimal client flake for a custom Rift devbox image.
#
# Copy this into your repository, add your tools to `extraModules`, and Rift's
# managed builder will build it with a pure `nix build .#fixed-labs.rift.image`.
# See docs/image-config.md for the full walkthrough.
{
  description = "Custom Rift devbox image";

  inputs.rift.url = "github:fixed-labs/oss";

  outputs =
    { self, rift, ... }:
    {
      # Managed builds read exactly this output — the versioned `fixed-labs.rift`
      # contract ({ version; image; }). It lives at the custom top-level path
      # `fixed-labs.rift`, NOT under `packages`, so a bare `nix build` / `nix
      # flake check` leaves your repo's real artifacts alone.
      fixed-labs.rift = rift.lib.mkRift {
        # `self` is the checked-out tree. mkRift defaults repoSrc = self and
        # imageCommit = self.rev or null from it, so this repo's source is baked
        # without restating those (an impure `builtins.getEnv` pattern would
        # silently bake nothing under the pure managed build).
        inherit self;

        extraModules = [
          # The devboxes-base substrate (overlay-root boot, WireGuard, the agent).
          # mkDevimage already bakes this in — listing it here is optional, shown
          # to make the layering explicit (the module system dedupes it).
          rift.nixosModules.devboxes-base

          # Your customization: tools, editors, dotfiles, base-module options.
          (
            { pkgs, ... }:
            {
              environment.systemPackages = [
                pkgs.go
                pkgs.ripgrep
                pkgs.jq
              ];

              # Configure the base via its options if you like:
              # rift.devboxes-base.loginUser = "dev";
            }
          )
        ];
      };
    };
}
