{
  # A plausible PRE-EXISTING flake, before Rift adoption. It has its own
  # `inputs` (nixpkgs), its own `outputs` (a `packages` build artifact and a
  # devShell), and an `outputs` arg set that does NOT yet bind `rift`. The (c)
  # fixture of the rift-init eval spine test does NOT eval this file — it evals
  # the hand-merged result in expected-merged/flake.nix. This file is kept as
  # the "before" so the merge (the model's runtime job, out of CI scope) is
  # legible next to its expected output.
  description = "an existing project, pre-Rift";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      pkgs = nixpkgs.legacyPackages.x86_64-linux;
    in
    {
      packages.x86_64-linux.hello = pkgs.hello;

      devShells.x86_64-linux.default = pkgs.mkShell {
        packages = [ pkgs.go ];
      };
    };
}
