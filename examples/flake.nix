# Minimal client flake for a custom Rift devbox image.
#
# Copy this into your repository, add your tools to `extraModules`, and Rift's
# managed builder will build it with a pure `nix build .#devimage`. See
# docs/image-config.md for the full walkthrough.
{
  description = "Custom Rift devbox image";

  inputs.rift.url = "github:fixed-labs/oss";

  outputs =
    { self, rift, ... }:
    {
      # Managed builds build exactly this attribute.
      packages.x86_64-linux.devimage = rift.lib.mkDevimage {
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

        # Bake this repo's source into the image. Under the pure managed build,
        # `self` is the correct handle to the checked-out tree — an impure
        # `builtins.getEnv` pattern would silently bake nothing.
        repoSrc = self;
        imageCommit = self.rev or null;
      };
    };
}
