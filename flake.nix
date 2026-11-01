{
  description = "UPnP port forwarding for Docker containers and systemd units";

  # The release nix-config builds every host from, which has go_1_27, the Go
  # that go.mod asks for and mise.toml pins.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # nixpkgs' convention for a build from a commit rather than a release:
      # the commit's date, so it never claims to be one of ./run's versions,
      # and it is the same however the flake was fetched (a github: input has
      # no commit count).
      date = self.lastModifiedDate or "19700101";
      version = "0-unstable-${builtins.substring 0 4 date}-${builtins.substring 4 2 date}-${builtins.substring 6 2 date}";

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
