{
  description = "The Sourcegraph developer environment & packages Nix Flake";

  inputs = {
    nixpkgs.url = "nixpkgs/nixos-unstable";
    utils.url = "github:numtide/flake-utils";
    ocaml.url = "github:nix-ocaml/nix-overlays?rev=6b007cfca815e3f94b692bdf4cd081424446c003";
  };

  outputs = { self, nixpkgs, utils, ocaml }:
    with nixpkgs.lib; with utils.lib; {
      devShells = eachDefaultSystem (system:
        let
          pkgs = import nixpkgs { inherit system; overlays = [ self.overlays.ctags ]; };
        in
        {
          default = pkgs.callPackage ./shell.nix { };
        }
      );

      # Pin a specific version of universal-ctags to the same version as in cmd/symbols/ctags-install-alpine.sh.
      overlays.ctags = (import ./dev/nix/ctags.nix { inherit nixpkgs utils; }).overlay;

      packages = fold recursiveUpdate { } [
        ((import ./dev/nix/ctags.nix { inherit nixpkgs utils; }).packages)
        (import ./dev/nix/p4-fusion.nix { inherit nixpkgs utils; })
        (import ./dev/nix/comby.nix { inherit utils; nixpkgs = ocaml; })
      ];
    };
}
