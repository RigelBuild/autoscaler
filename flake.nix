{
  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs?ref=master";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    { nixpkgs, flake-utils, ... }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        lib = pkgs.lib;

        version = lib.removeSuffix "\n" (builtins.readFile ./.github/rigel-version);
        pins = import ./nix/versions/autoscaler.nix { inherit lib; };

        # Only platforms with a published release asset get a package.
        packages = lib.optionalAttrs (pins.assets ? ${system}) (
          let
            asset = pins.assets.${system};
            woodpecker-autoscaler = pkgs.stdenvNoCC.mkDerivation {
              pname = "woodpecker-autoscaler";
              inherit version;

              # Fetched from the release published by release-binaries.yaml.
              src = pkgs.fetchurl {
                url = "https://github.com/RigelBuild/autoscaler/releases/download/v${version}/woodpecker-autoscaler_${version}_${asset.name}";
                inherit (asset) hash;
              };

              dontUnpack = true;
              dontBuild = true;

              installPhase = ''
                runHook preInstall
                install -Dm755 "$src" "$out/bin/woodpecker-autoscaler"
                runHook postInstall
              '';

              meta = {
                description = "Woodpecker autoscaler, release binary";
                homepage = "https://github.com/RigelBuild/autoscaler";
                mainProgram = "woodpecker-autoscaler";
                platforms = [ system ];
              };
            };
          in
          {
            inherit woodpecker-autoscaler;
            default = woodpecker-autoscaler;
          }
        );
      in
      {
        inherit packages;

        devShells.default =
          with pkgs;
          let
            go = go_1_27;
          in
          pkgs.mkShell {
            buildInputs = [
              gnumake
              go
              glibc.static
              gofumpt
              golangci-lint
              go-mockery
              addlicense
              gcc
            ];
            CFLAGS = "-I${pkgs.glibc.dev}/include";
            LDFLAGS = "-L${pkgs.glibc}/lib";
            GO = "${go}/bin/go";
            GOROOT = "${go}/share/go";
          };
      }
    );
}
