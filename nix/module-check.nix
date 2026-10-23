# Evaluates a configuration that uses the module and checks what it renders,
# without booting anything - the part of the VM test that needs no KVM.
{ pkgs, self }:

let
  system = pkgs.nixos {
    imports = [ self.nixosModules.default ];
    boot.loader.grub.enable = false;
    fileSystems."/" = {
      device = "none";
      fsType = "tmpfs";
    };
    system.stateVersion = "25.11";

    services.portical = {
      enable = true;
      root = "http://192.168.1.1:5000/rootDesc.xml";
      sources = [
        "systemd"
        "docker"
      ];
      openFirewall = true;
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
in
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

    grep -q -- '--source systemd,docker' <<<"$portical" || fail "sources not passed"
    grep -q -- '--root http://192.168.1.1:5000/rootDesc.xml' <<<"$portical" || fail "root not passed"
    grep -q -- '--docker-socket /run/docker.sock' <<<"$portical" || fail "docker socket not passed"
    grep -qx 'DynamicUser=true' <<<"$portical" || fail "not a dynamic user"
    grep -qx 'SupplementaryGroups=docker' <<<"$portical" || fail "not in the docker group"

    # openFirewall: the internal ports, both protocols for a bare port.
    [[ " $tcp " == *" 25565 "* ]] || fail "25565/tcp not opened: $tcp"
    [[ " $udp " == *" 25565 "* && " $udp " == *" 19132 "* ]] || fail "udp not opened: $udp"
    [[ " $udp " != *" 19133 "* ]] || fail "opened the external port 19133"

    touch $out
  ''
