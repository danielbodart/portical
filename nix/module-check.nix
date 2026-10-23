# Evaluates a configuration that uses the module and checks what it renders,
# without booting anything - the part of the VM test that needs no KVM.
{ pkgs, self }:

let
  inherit (pkgs) lib;

  base = {
    imports = [ self.nixosModules.default ];
    boot.loader.grub.enable = false;
    fileSystems."/" = {
      device = "none";
      fsType = "tmpfs";
    };
    system.stateVersion = "25.11";
  };

  system = pkgs.nixos {
    imports = [ base ];
    # What virtualisation.docker would define, without building Docker.
    users.groups.docker = { };

    services.portical = {
      enable = true;
      root = "http://192.168.1.1:5000/rootDesc.xml";
      sources = [
        "systemd"
        "docker"
      ];
      openFirewall = true;
      extraArgs = [
        "--label"
        "a%nb$HOME"
      ];
    };

    systemd.services.minecraft = {
      wantedBy = [ "multi-user.target" ];
      serviceConfig.ExecStart = "${pkgs.coreutils}/bin/sleep infinity";
      portical.forward = [
        "25565"
        "19133:19132/udp"
        "published"
      ];
    };
  };
  inherit (system.config) systemd networking;

  # Whether a configuration fails to evaluate, or fails an assertion.
  refused =
    config:
    let
      evaluated = (pkgs.nixos { imports = [ base config ]; }).config;
      attempt = builtins.tryEval (
        builtins.deepSeq (map (unit: unit.text) (lib.attrValues evaluated.systemd.units)) (
          lib.any (assertion: !assertion.assertion) evaluated.assertions
        )
      );
    in
    !attempt.success || attempt.value;

  forwarding = forward: {
    systemd.services.game = {
      serviceConfig.ExecStart = "${pkgs.coreutils}/bin/sleep infinity";
      portical.forward = forward;
    };
  };

  # Each must be refused when evaluated, not later by Portical at run time.
  refusals = {
    "port 0" = forwarding [ "0" ];
    "port 70000" = forwarding [ "70000/tcp" ];
    "internal port 65536" = forwarding [ "80:65536" ];
    "a word" = forwarding [ "http" ];
    "the docker source without a docker group" = {
      services.portical = {
        enable = true;
        sources = [ "docker" ];
      };
    };
  };
  accepted = {
    "ports 1 and 65535" = forwarding [
      "1"
      "65535:1/udp"
    ];
    "the docker source with a docker group" = {
      users.groups.docker = { };
      services.portical = {
        enable = true;
        sources = [ "docker" ];
      };
    };
  };
  wrong =
    lib.attrNames (lib.filterAttrs (_: config: !refused config) refusals)
    ++ lib.attrNames (lib.filterAttrs (_: refused) accepted);
in
assert lib.assertMsg (wrong == [ ]) "module-check: wrongly accepted or refused: ${lib.concatStringsSep ", " wrong}";
pkgs.runCommand "portical-module-check"
  {
    minecraft = systemd.units."minecraft.service".text;
    portical = systemd.units."portical.service".text;
    tcp = toString networking.firewall.allowedTCPPorts;
    udp = toString networking.firewall.allowedUDPPorts;
  }
  ''
    fail() { echo "FAIL: $*"; exit 1; }

    grep -qx 'X-Portical-Forward=25565' <<<"$minecraft" || fail "minecraft.service has no 25565"
    grep -qx 'X-Portical-Forward=19133:19132/udp' <<<"$minecraft" || fail "minecraft.service has no 19133"
    grep -qx 'X-Portical-Forward=published' <<<"$minecraft" || fail "minecraft.service has no published"
    # In [Unit], before [Service] starts.
    sed '/^\[Service\]/q' <<<"$minecraft" | grep -q X-Portical-Forward || fail "not in [Unit]"

    grep -qF -- '"--source" "systemd,docker"' <<<"$portical" || fail "sources not passed"
    grep -qF -- '"--root" "http://192.168.1.1:5000/rootDesc.xml"' <<<"$portical" || fail "root not passed"
    grep -qF -- '"--docker-socket" "/run/docker.sock"' <<<"$portical" || fail "docker socket not passed"
    # Escaped for systemd, which would otherwise expand %n and $HOME.
    grep -qF -- '"--label" "a%%nb$$HOME"' <<<"$portical" || fail "extraArgs not escaped for systemd"
    grep -qx 'DynamicUser=true' <<<"$portical" || fail "not a dynamic user"
    grep -qx 'SupplementaryGroups=docker' <<<"$portical" || fail "not in the docker group"

    # openFirewall: the internal ports, both protocols for a bare port.
    [[ " $tcp " == *" 25565 "* ]] || fail "25565/tcp not opened: $tcp"
    [[ " $udp " == *" 25565 "* && " $udp " == *" 19132 "* ]] || fail "udp not opened: $udp"
    [[ " $udp " != *" 19133 "* ]] || fail "opened the external port 19133"

    touch $out
  ''
