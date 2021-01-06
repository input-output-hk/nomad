{
  description = "Flake for Nomad";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-22.11";
    utils.url = "github:numtide/flake-utils";
  };

  outputs = {
    self,
    nixpkgs,
    utils,
    nix,
  }: (utils.lib.eachSystem ["x86_64-linux" "x86_64-darwin"] (system: let
    overlay = final: prev: {
      go = prev.go_1_19;
      nomad = final.buildGoModule {
        pname = "nomad";
        version = "1.4.3";

        subPackages = ["."];

        src = ./.;

        vendorSha256 = "sha256-JQRpsQhq5r/QcgFwtnptmvnjBEhdCFrXFrTKkJioL3A=";

        # ui:
        #  Nomad release commits include the compiled version of the UI, but the file
        #  is only included if we build with the ui tag.
        # nonvidia:
        #  We disable Nvidia GPU scheduling on Linux, as it doesn't work there:
        #  Ref: https://github.com/hashicorp/nomad/issues/5535
        preBuild = let
          tags = ["ui"] ++ prev.lib.optional prev.stdenv.isLinux "nonvidia";
          tagsString = prev.lib.concatStringsSep " " tags;
        in ''
          export buildFlagsArray=(
            -tags="${tagsString}"
          )
        '';

        meta = with prev.lib; {
          homepage = "https://www.nomadproject.io/";
          description = "A Distributed, Highly Available, Datacenter-Aware Scheduler";
          platforms = platforms.unix;
          license = licenses.mpl20;
          maintainers = with maintainers; [manveru];
        };
      };
    };

    pkgs = import nixpkgs {
      inherit system;
      overlays = [overlay];
    };
  in {
    inherit overlay;

    packages = {inherit (pkgs) nomad;};
    defaultPackage = pkgs.nomad;

    devShell = pkgs.mkShell {
      buildInputs = with pkgs; [go gotools gopls gocode];
    };
  }));
}
