{
  description = "rift — the developer CLI, in-box agent, and base image for the hosted service";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    { self, nixpkgs }:
    let
      oss = import ./default.nix { inherit nixpkgs; };
      forAllSystems = nixpkgs.lib.genAttrs [
        "x86_64-linux"
        "aarch64-linux"
      ];
    in
    {
      # The `rift` CLI and the in-box agent, per system. These plus
      # nixosModules.devboxes-base and lib.mkDevimage are the contract.
      packages =
        forAllSystems (system: {
          inherit (oss.packagesFor system) rift agent;
        })
        // {
          # Smoke-test image only: exposed under x86_64-linux alone, since
          # `mkDevimage {}` defaults hostSystem = x86_64 and an aarch64 devimage
          # attr would demand binfmt. Bakes no extraModules, so no real client
          # copies it — clients template from examples/flake.nix.
          x86_64-linux = {
            inherit (oss.packagesFor "x86_64-linux") rift agent;
            devimage = oss.lib.mkDevimage { };
          };
        };

      # The base image NixOS module a client repo imports and extends.
      nixosModules.devboxes-base = oss.nixosModules.devboxes-base;

      # The image packer a client calls to build its own devbox image (the
      # low-level primitive).
      lib.mkDevimage = oss.lib.mkDevimage;

      # The versioned `fixed-labs.rift` contract helper a consumer calls: wraps
      # mkDevimage into the `{ version = 1; image = … }` envelope the managed
      # builder reads, defaulting repoSrc/imageCommit from the consumer's `self`.
      lib.mkRift = oss.lib.mkRift;
    };
}
