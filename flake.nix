{
  description = "Flake for Nomad";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-20.09";
    nix.url = "github:NixOS/nix";
    utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, utils, nix }:
    (utils.lib.eachSystem [ "x86_64-linux" "x86_64-darwin" ] (system: rec {
      overlay = final: prev: { nix = nix.packages.${system}.nix; };

      legacyPackages = import nixpkgs {
        inherit system;
        overlays = [ overlay ];
      };

      packages = {
      };

      devShell = legacyPackages.mkShell {
        buildInputs = with legacyPackages; [ go goimports gopls gocode ];
      };
    }));
}
