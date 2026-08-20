{
  description = "litefind - grep for SQLite dbs";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs, ... }:
    let
      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      pkgsFor = system: import nixpkgs { inherit system; };
      packageVersion = nixpkgs.lib.removeSuffix "\n" (builtins.readFile ./VERSION);
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = pkgsFor system;
        in
        {
          default = pkgs.buildGoModule {
            pname = "litefind";
            version = packageVersion;
            src = ./.;
            vendorHash = "sha256-BAvfNq8jRMtxnNRnCfD4m3N9Yqc7o9dM/v6eVfK0Iag=";
            ldflags = [
              "-s"
              "-w"
              "-X 'main.versionOverride=${packageVersion}'"
            ];
            meta = {
              description = "litefind - grep for SQLite dbs";
              license = pkgs.lib.licenses.mit;
              mainProgram = "litefind";
            };
          };
        }
      );

      apps = forAllSystems (system: {
        default = {
          type = "app";
          program = "${nixpkgs.lib.getExe self.packages.${system}.default}";
          meta = self.packages.${system}.default.meta;
        };
      });

      devShells = forAllSystems (
        system:
        let
          pkgs = pkgsFor system;
        in
        {
          default = pkgs.mkShell {
            packages = [
              pkgs.go_1_26
              pkgs.golangci-lint
              pkgs.goreleaser
            ];
          };
        }
      );

      formatter = forAllSystems (system: (pkgsFor system).nixfmt);
    };
}
