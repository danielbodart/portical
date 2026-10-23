# services.portical, and `portical.forward` on every systemd service.
#
# Takes the flake's own packages, so a configuration that imports
# nixosModules.default runs the Portical it was built from by default.
{ packages }:
{
  config,
  lib,
  pkgs,
  utils,
  ...
}:

let
  inherit (lib)
    concatLists
    concatMap
    filter
    mapAttrsToList
    mkEnableOption
    mkIf
    mkOption
    optional
    optionals
    types
    unique
    ;

  cfg = config.services.portical;

  # The label's grammar, one term per element, with every port in 1-65535 as
  # internal/rules has it. Checked here so that a typo fails the evaluation,
  # against the service that has it, rather than turning up as a warning in
  # the journal.
  term =
    let
      syntax = types.strMatching "published|[0-9]{1,5}(:[0-9]{1,5})?(/(tcp|udp))?";
      # The external and internal port, either of which may be absent.
      ports =
        value:
        let
          match = builtins.match "([0-9]+)(:([0-9]+))?.*" value;
        in
        if match == null then [ ] else [ (builtins.elemAt match 0) (builtins.elemAt match 2) ];
      inRange =
        value:
        lib.all (port: port >= 1 && port <= 65535) (
          map lib.toIntBase10 (filter (port: port != null) (ports value))
        );
    in
    types.addCheck syntax inRange
    // {
      description = "${syntax.description}, with ports from 1 to 65535";
    };

  # The ports a term opens on this host: the internal port, for each protocol
  # it names. `published` is whatever the unit listens on, which is not
  # known until it runs, so it opens nothing.
  opened =
    value:
    let
      match = builtins.match "([0-9]+)(:([0-9]+))?(/(tcp|udp))?" value;
      external = builtins.elemAt match 0;
      internal = builtins.elemAt match 2;
      protocol = builtins.elemAt match 4;
      port = lib.toIntBase10 (if internal == null then external else internal);
    in
    if match == null then
      [ ]
    else
      map (protocol: { inherit port protocol; }) (
        if protocol == null then
          [
            "tcp"
            "udp"
          ]
        else
          [ protocol ]
      );

  ports = concatLists (
    mapAttrsToList (_: service: concatMap opened service.portical.forward) config.systemd.services
  );
  portsFor = protocol: unique (map (p: p.port) (filter (p: p.protocol == protocol) ports));
in
{
  options.services.portical = {
    enable = mkEnableOption "Portical, UPnP port forwarding for systemd units and Docker containers";

    package = mkOption {
      type = types.package;
      default = packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalMD "this flake's package";
      description = "The Portical to run.";
    };

    root = mkOption {
      type = types.nullOr types.str;
      default = null;
      example = "http://192.168.1.1:5000/rootDesc.xml";
      description = ''
        The gateway's UPnP root description URL. When null, Portical searches
        for one over SSDP - but the NixOS firewall usually drops the gateway's
        reply, which comes from its own address to an ephemeral port that
        conntrack does not tie to the multicast search. Setting this is
        recommended.
      '';
    };

    sources = mkOption {
      type = types.nonEmptyListOf (
        types.enum [
          "systemd"
          "docker"
        ]
      );
      default = [ "systemd" ];
      example = [
        "systemd"
        "docker"
      ];
      description = ''
        Where forwards are declared. Run one Portical with every source the
        host needs, rather than one per source: two on the same host each
        remove the other's mappings. The first named wins a port both ask for.
      '';
    };

    interval = mkOption {
      type = types.ints.positive;
      default = 15;
      description = "Seconds between reconcile passes.";
    };

    leaseDuration = mkOption {
      type = types.ints.unsigned;
      default = 0;
      description = "Lease to request, in seconds. 0 never expires.";
    };

    renewWithin = mkOption {
      type = types.ints.unsigned;
      default = 43200;
      description = "Renew a mapping whose lease expires within this many seconds.";
    };

    steal = mkOption {
      type = types.bool;
      default = false;
      description = "Take over an external port another tool already forwards.";
    };

    manageAll = mkOption {
      type = types.bool;
      default = false;
      description = ''
        Manage every Portical mapping, wherever it points. Only safe with one
        Portical on the network.
      '';
    };

    cleanupOnExit = mkOption {
      type = types.bool;
      default = false;
      description = "Remove Portical's mappings when the service stops.";
    };

    dockerSocket = mkOption {
      type = types.str;
      default = "/run/docker.sock";
      description = "The Docker socket, for the docker source.";
    };

    openFirewall = mkOption {
      type = types.bool;
      default = false;
      description = ''
        Open the internal port of every service's `portical.forward` in
        networking.firewall. `published` opens nothing, since what a unit
        listens on is only known once it runs.
      '';
    };

    extraArgs = mkOption {
      type = types.listOf types.str;
      default = [ ];
      example = [ "--dry-run" ];
      description = "Further command line arguments, before the command.";
    };
  };

  # Extends the submodule every systemd.services.<name> already is, the way
  # nixpkgs' confinement module adds `confinement`: declaring the option again
  # merges these into it. Declared whether or not Portical is enabled here, so
  # a service can say what it wants forwarded wherever it is defined.
  options.systemd.services = mkOption {
    type = types.attrsOf (
      types.submodule (
        { config, ... }:
        {
          options.portical.forward = mkOption {
            type = types.listOf term;
            default = [ ];
            example = [
              "25565"
              "19133:19132/udp"
            ];
            description = ''
              Ports for Portical to forward to this service while it is active,
              in the label's grammar: `external[:internal][/protocol]`, or
              `published` for whatever the service listens on. Rendered as
              `X-Portical-Forward=` lines in its [Unit] section.
            '';
          };

          config = mkIf (config.portical.forward != [ ]) {
            unitConfig."X-Portical-Forward" = config.portical.forward;
          };
        }
      )
    );
  };

  config = mkIf cfg.enable {
    assertions = [
      {
        # SupplementaryGroups=docker would otherwise fail the service at
        # start, 216/GROUP, rather than the build.
        assertion = !(builtins.elem "docker" cfg.sources) || config.users.groups ? docker;
        message = ''
          services.portical.sources includes "docker", but there is no docker group:
          enable virtualisation.docker, or define users.groups.docker for a Docker
          installed another way.
        '';
      }
    ];

    networking.firewall = mkIf cfg.openFirewall {
      allowedTCPPorts = portsFor "tcp";
      allowedUDPPorts = portsFor "udp";
    };

    systemd.services.portical = {
      description = "Portical UPnP port forwarding";
      documentation = [ "https://github.com/danielbodart/portical" ];
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ] ++ optional (builtins.elem "docker" cfg.sources) "docker.service";

      serviceConfig = {
        # Escaped for systemd rather than a shell: it expands % specifiers
        # and $VARIABLES even inside quotes.
        ExecStart = utils.escapeSystemdExecArgs (
          [
            (lib.getExe cfg.package)
            "--source"
            (lib.concatStringsSep "," cfg.sources)
            "--duration"
            (toString cfg.interval)
            "--lease"
            (toString cfg.leaseDuration)
            "--renew-within"
            (toString cfg.renewWithin)
          ]
          ++ optionals (cfg.root != null) [
            "--root"
            cfg.root
          ]
          ++ optionals (builtins.elem "docker" cfg.sources) [
            "--docker-socket"
            cfg.dockerSocket
          ]
          ++ optional cfg.steal "--steal"
          ++ optional cfg.manageAll "--manage-all"
          ++ optional cfg.cleanupOnExit "--cleanup-on-exit"
          ++ cfg.extraArgs
          ++ [ "run" ]
        );
        Restart = "on-failure";
        RestartSec = 5;

        DynamicUser = true;
        # Docker's socket is group-owned; the relay for macvlan containers
        # runs Portical's own image, which a native Portical cannot detect,
        # so pass --helper-image in extraArgs for that case.
        SupplementaryGroups = optional (builtins.elem "docker" cfg.sources) "docker";

        # Nothing here needs privilege. The system bus lets any user list
        # units, read their properties and subscribe; unit files live in
        # /etc, /run and /nix/store, all readable; and listing sockets over
        # NETLINK_SOCK_DIAG only reads, which any user may.
        CapabilityBoundingSet = "";
        AmbientCapabilities = "";
        NoNewPrivileges = true;
        # The gateway and SSDP; the system bus and Docker; sock_diag for
        # `published`.
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
          "AF_NETLINK"
        ];
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        # Read-only: `published` walks /sys/fs/cgroup to name a socket's
        # cgroup, and reading is all it does there.
        ProtectControlGroups = true;
        ProtectClock = true;
        ProtectHostname = true;
        ProtectProc = "invisible";
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = [
          "@system-service"
          "~@privileged"
        ];
        UMask = "0077";
        # A Go binary has no JIT, so no memory is ever both written and run.
        MemoryDenyWriteExecute = true;
        # Not PrivateNetwork: the whole point is the LAN.
      };
    };
  };
}
