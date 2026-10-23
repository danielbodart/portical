{
  description = "UPnP port forwarding for Docker containers and systemd units";

  # Unstable for buildGo127Module, the Go that go.mod asks for and mise.toml
  # pins - the stable branch has no Go 1.27 yet.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # The same scheme as ./run: the committed major, the commit count, and
      # the commit's date in place of a CI run number. A dirty tree has no
      # count, and says 0.
      major = lib.fileContents ./MAJOR;
      version = "${major}.${toString (self.revCount or 0)}.${self.lastModifiedDate or "0"}";

      src = lib.fileset.toSource {
        root = ./.;
        # go.mod, go.sum and every .go file, tests included, and the fixture
        # the tests read; so editing the docs or the flake does not rebuild it.
        fileset = lib.fileset.unions [
          ./go.mod
          ./go.sum
          (lib.fileset.fileFilter (file: file.hasExt "go") ./.)
          ./internal/systemd/testdata
        ];
      };
    in
    {
      packages = forAllSystems (pkgs: {
        default = pkgs.callPackage ./nix/package.nix { inherit version src; };
      });

      nixosModules.default = import ./nix/module.nix { inherit (self) packages; };

      checks = forAllSystems (pkgs: {
        # Its check phase is `go test`, so building it runs the tests.
        package = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
        module = import ./nix/module-check.nix { inherit pkgs self; };
        vm = import ./nix/vm-test.nix { inherit pkgs self; };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
