{
  # The HAND-MERGED result of adopting Rift into the sibling ../flake.nix
  # (fixture (c) of the rift-init eval spine test evals THIS file). The merge —
  # the model's runtime Splice step (plan Phase-1 step 4) — did three things to
  # the pre-existing flake:
  #   1. added `inputs.rift.url = "github:fixed-labs/oss"` (emit's inputs piece),
  #   2. bound `rift` in the `outputs` arg set (it also gained a `...` so the
  #      set stays open), and
  #   3. added the `fixed-labs.rift = rift.lib.mkRift { ... }` output (emit's
  #      outputs piece).
  # The pre-existing `nixpkgs` input, `packages`, and `devShells` are preserved
  # verbatim. The test evals `.#fixed-labs.rift.image.drvPath` (eval-only, no
  # build) with the `rift` input overridden to the LOCAL oss (which has mkRift)
  # and `nixpkgs` overridden to keep the eval self-contained.
  description = "an existing project, pre-Rift";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  inputs.rift.url = "github:fixed-labs/oss";

  outputs =
    {
      self,
      nixpkgs,
      rift,
      ...
    }:
    let
      pkgs = nixpkgs.legacyPackages.x86_64-linux;
    in
    {
      packages.x86_64-linux.hello = pkgs.hello;

      devShells.x86_64-linux.default = pkgs.mkShell {
        packages = [ pkgs.go ];
      };

      fixed-labs.rift = rift.lib.mkRift {
        inherit self;
        extraModules = [
          (
            { pkgs, ... }:
            {
              environment.systemPackages = [ pkgs.go ];
            }
          )
        ];
      };
    };
}
