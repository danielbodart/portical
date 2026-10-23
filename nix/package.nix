{
  lib,
  buildGo127Module,
  version,
  src,
}:

# The same static build as ./run, by nixpkgs' Go, and the check phase runs the
# tests on the way. buildGo127Module rather than buildGoModule, which is still
# Go 1.26 in the pinned nixpkgs and would refuse go.mod's `go 1.27`.
#
# Nothing goes on PATH any more: the systemd source's `published` asks the
# kernel for listening sockets over netlink itself, where it used to run ss.
buildGo127Module {
  pname = "portical";
  inherit version src;

  # godbus and the golang.org/x/sys it needs, as `go mod vendor` lays them
  # out. Computed without a Nix daemon - `go mod vendor` into a copy of the
  # tree, then `nix-hash --type sha256 --sri vendor` - so CI's nix job is the
  # first real check of it. A go.sum change needs it recomputed: build once
  # with lib.fakeHash and take the hash Nix reports.
  vendorHash = "sha256-y+3dL8WEiN4PDHLX9/iNijS20wUheiJ+mZrM7af69G0=";

  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
    "-X github.com/danielbodart/portical/internal/version.Version=${version}"
  ];

  meta = {
    description = "UPnP port forwarding for Docker containers and systemd units";
    homepage = "https://github.com/danielbodart/portical";
    license = lib.licenses.asl20;
    platforms = [
      "x86_64-linux"
      "aarch64-linux"
      "armv7l-linux"
    ];
    mainProgram = "portical";
  };
}
