{
  lib,
  buildGoModule,
  go_1_27,
  version,
  src,
}:

# The same static build as ./run, by nixpkgs' Go, and the check phase runs the
# tests on the way. With go_1_27, the major.minor mise.toml pins, rather than
# nixpkgs' default Go, which is 1.26 and would refuse go.mod's `go 1.27`.
#
# Nothing goes on PATH any more: the systemd source's `published` asks the
# kernel for listening sockets over netlink itself, where it used to run ss.
(buildGoModule.override { go = go_1_27; }) {
  pname = "portical";
  inherit version src;

  # godbus, gokit, gokit/dbusx and the golang.org/x/sys godbus needs, as
  # `go mod vendor` lays them out. A go.sum change needs it recomputed: build once with
  # lib.fakeHash and take the hash Nix reports.
  vendorHash = "sha256-QmQCZNC0mod6lZXMlq6CTtq+V6QAuNUZPbrGXOrJgms=";

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
