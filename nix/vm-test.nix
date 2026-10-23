# A router running miniupnpd, a host running Portical on its systemd source,
# and a client on the far side of the router to prove a forward carries
# traffic. Laid out as nixpkgs' own upnp test is.
#
# Portical's interval is set to an hour, so every change after the first pass
# has to arrive as a systemd event: a unit stopping, and a daemon-reload.
{ pkgs, self }:

let
  routerInternal = "192.168.3.1";
  routerExternal = "80.100.100.1";
  hostAddress = "192.168.3.2";
  clientAddress = "80.100.100.2";
  root = "http://${routerInternal}:5000/rootDesc.xml";

  listen =
    port:
    "${pkgs.python3}/bin/python3 -m http.server --directory ${pkgs.writeTextDir "index.html" "port ${toString port}"} ${toString port}";
in
pkgs.testers.runNixOSTest {
  name = "portical";

  nodes = {
    router = {
      virtualisation.vlans = [
        1
        2
      ];
      networking.nat = {
        enable = true;
        internalInterfaces = [ "eth2" ];
        externalInterface = "eth1";
      };
      # miniupnpd's iptables backend cannot find iptables in the VM.
      networking.nftables.enable = true;
      networking.firewall.trustedInterfaces = [ "eth2" ];
      networking.interfaces.eth1.ipv4.addresses = [
        {
          address = routerExternal;
          prefixLength = 24;
        }
      ];
      networking.interfaces.eth2.ipv4.addresses = [
        {
          address = routerInternal;
          prefixLength = 24;
        }
      ];
      services.miniupnpd = {
        enable = true;
        externalInterface = "eth1";
        internalIPs = [ "eth2" ];
        appendConfig = ''
          ext_ip=${routerExternal}
          http_port=5000
        '';
      };
    };

    host = {
      imports = [ self.nixosModules.default ];
      virtualisation.vlans = [ 2 ];
      networking.defaultGateway = routerInternal;
      networking.interfaces.eth1.ipv4.addresses = [
        {
          address = hostAddress;
          prefixLength = 24;
        }
      ];
      environment.systemPackages = [ pkgs.miniupnpc ];

      services.portical = {
        enable = true;
        inherit root;
        interval = 3600;
        openFirewall = true;
      };

      # Forwarded by an explicit term, and its port opened by openFirewall.
      systemd.services.explicit = {
        wantedBy = [ "multi-user.target" ];
        serviceConfig.ExecStart = listen 8080;
        portical.forward = [ "8080/tcp" ];
      };

      # Forwarded for whatever it listens on.
      systemd.services.listener = {
        wantedBy = [ "multi-user.target" ];
        serviceConfig.ExecStart = listen 9090;
        portical.forward = [ "published" ];
      };
      networking.firewall.allowedTCPPorts = [ 9090 ];
    };

    client = {
      virtualisation.vlans = [ 1 ];
      networking.interfaces.eth1.ipv4.addresses = [
        {
          address = clientAddress;
          prefixLength = 24;
        }
      ];
    };
  };

  testScript = ''
    def mappings():
        return host.succeed("upnpc -u ${root} -l")

    def wait_for_mapping(port, present=True):
        def check(_):
            listed = mappings()
            found = any(
                f"{port}->${hostAddress}:{port}" in line and "portical:" in line
                for line in listed.splitlines()
            )
            return found == present
        retry(check, timeout_seconds=60)

    start_all()
    router.wait_for_unit("miniupnpd.service")
    router.wait_for_open_port(5000, "${routerInternal}")

    host.wait_for_unit("multi-user.target")
    host.succeed("systemctl cat explicit.service | grep -x 'X-Portical-Forward=8080/tcp'")

    # `published` only sees a port once the unit is listening, and a
    # listener that opens late is caught by the interval rather than an
    # event - an hour here. So the first pass is made to come after it.
    host.wait_for_open_port(8080)
    host.wait_for_open_port(9090)
    host.systemctl("restart portical.service")
    # Subscribed only once the first pass is done: a unit that stops before
    # then is not seen until the next interval.
    host.wait_until_succeeds(
        "journalctl -u portical.service "
        "_SYSTEMD_INVOCATION_ID=$(systemctl show -p InvocationID --value portical.service) "
        "| grep 'Watching for changes'"
    )

    with subtest("forwards an explicit term and a published port"):
        wait_for_mapping(8080)
        wait_for_mapping(9090)
        print(mappings())

    with subtest("the forward carries traffic from outside"):
        client.wait_until_succeeds("curl -sf http://${routerExternal}:8080/ | grep 'port 8080'")
        client.wait_until_succeeds("curl -sf http://${routerExternal}:9090/ | grep 'port 9090'")

    with subtest("a unit stopping removes its forward, by event"):
        host.systemctl("stop explicit.service")
        wait_for_mapping(8080, present=False)
        host.wait_until_succeeds("journalctl -u portical.service | grep 'Unit explicit.service inactive'")

    with subtest("a drop-in resetting the key removes its forward, on daemon-reload"):
        # One command: a pass between writing the drop-in and the reload
        # would rightly warn that the unit's files are newer than systemd's.
        host.succeed(
            "mkdir -p /run/systemd/system/listener.service.d"
            " && printf '[Unit]\\nX-Portical-Forward=\\n' > /run/systemd/system/listener.service.d/off.conf"
            " && systemctl daemon-reload"
        )
        wait_for_mapping(9090, present=False)

    with subtest("runs unprivileged, and says nothing went wrong"):
        host.succeed("test $(stat -c %u /proc/$(systemctl show -p MainPID --value portical.service)) -ne 0")
        journal = host.succeed(
            "journalctl -u portical.service "
            "_SYSTEMD_INVOCATION_ID=$(systemctl show -p InvocationID --value portical.service)"
        )
        print(journal)
        assert "Error:" not in journal, "Portical logged an error"
        assert "Warning:" not in journal, "Portical logged a warning"
  '';
}
