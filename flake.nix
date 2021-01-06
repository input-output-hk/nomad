{
  description = "Flake for Nomad";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-21.11";
    nix.url = "github:NixOS/nix";
    utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, utils, nix }:
    (utils.lib.eachSystem [ "x86_64-linux" "x86_64-darwin" ] (system:
      let
        overlay = final: prev: {
          inherit (nix.packages."${system}") nix;
          go = prev.go_1_17;
          nomad = prev.callPackage ./package.nix { };
        };

        pkgs = import nixpkgs {
          inherit system;
          overlays = [ overlay ];
        };
      in {
        inherit overlay;

        packages = { inherit (pkgs) nomad; };
        defaultPackage = pkgs.nomad;

        devShell = pkgs.mkShell {
          buildInputs = with pkgs; [ go goimports gopls gocode ];
        };
      }));
}
