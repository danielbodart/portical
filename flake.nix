{
  description = "UPnP port forwarding for Docker containers and systemd units";

  # Unstable for its Bun, which is the one mise.toml pins - the stable branch
  # is a minor version behind.
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

      # The same scheme as run.ts: the committed major, the commit count, and
      # the commit's date in place of a CI run number. A dirty tree has no
      # count, and says 0.
      major = lib.head (lib.splitString "." (lib.importJSON ./package.json).version);
      version = "${major}.${toString (self.revCount or 0)}.${self.lastModifiedDate or "0"}";

      src = lib.fileset.toSource {
        root = ./.;
        fileset = lib.fileset.unions [
          ./package.json
          ./tsconfig.json
          ./src
          ./test
        ];
      };
    in
    {
      packages = forAllSystems (pkgs: {
        default = pkgs.callPackage ./nix/package.nix { inherit version src; };
      });

      nixosModules.default = import ./nix/module.nix { inherit (self) packages; };

      checks = forAllSystems (pkgs: {
        # Builds the binary and runs `bun test` on the way.
        package = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
        module = import ./nix/module-check.nix { inherit pkgs self; };
        vm = import ./nix/vm-test.nix { inherit pkgs self; };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
