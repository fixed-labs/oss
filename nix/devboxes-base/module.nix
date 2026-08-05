# devboxes-base — the REQUIRED substrate every devbox workspace image builds
# on. A client repo imports this NixOS module (flake output
# `nixosModules.devboxes-base`), layers its toolchain/editor/dotfiles on top,
# and builds an OCI image with `mkDevimage` — the base being mandatory IS the
# image contract: the overlay-root boot, wg0, the devboxes-agent (and its
# WG-identity SSH server), and Fly/OCI boot can't be omitted.
#
# A devbox is a general-purpose, single-tenant developer machine — one login
# user who owns the box ("persists like a real computer"). The base
# deliberately ships no tailscale, no editor/devcontainer injection, and no
# multi-tenant split.
#
# Boot model: Fly's init stays VM PID 1 and runs the image entrypoint
# (mkDevimage's init script), which removes any stale resolv.conf from the
# volume upper at its source, assembles the overlay root (image = RO lower,
# volume upper at /persist), captures the machine's RIFT_* env to
# /etc/devboxes/boot-env and its seeded nameservers to
# /etc/devboxes/boot-resolv.conf (systemd-in-a-child-pidns inherits no machine
# env, and the DNS capture backstops stage-2's own registration — see
# devboxes-resolv below), pivots, and execs this system's stage-2 under
# `unshare --pid --fork
# --mount-proc` — systemd then runs as ns-PID-1 (getpid()==1 ⇒ real init mode).
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.rift.devboxes-base;
in
{
  options.rift.devboxes-base = {
    agentPackage = lib.mkOption {
      type = lib.types.package;
      description = ''
        The agent package to bake into the image, built for the image's
        system. `mkDevimage` builds this from the bundled agent source and
        wires it automatically, so a client calling `mkDevimage` never sets it
        by hand; the standalone agent derivation is also exposed as the flake's
        `.#agent` output.
      '';
    };

    loginUser = lib.mkOption {
      type = lib.types.str;
      default = "dev";
      description = ''
        The single login user every authorized peer lands as — the client
        image owns this user's environment. Must match the `login_user` the
        control plane hands out in attach bundles (the default is "dev").
      '';
    };

    loginShell = lib.mkOption {
      type = lib.types.package;
      default = pkgs.bashInteractive;
      description = "The login user's shell; exported to the agent as RIFT_LOGIN_SHELL.";
    };

    repoDir = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "/home/dev/myrepo";
      description = ''
        Absolute path of the baked repo working tree. When set, an
        interactive login auto-cd's here so a fresh box opens on the code
        instead of an empty $HOME. null (the default) = no auto-cd — for
        images built with no repo baked in (a plain `mkDevimage { }` with
        repoSrc = null). The packer (mkDevimage) supplies this; the base
        never hardcodes a directory name, so the behaviour is agnostic to
        what the client's repo is called.
      '';
    };
  };

  config = {
    # OCI/Fly boot: no kernel/initrd/bootloader of our own (Fly's microVM
    # kernel + our pidns systemd). isContainer trims NixOS to exactly that
    # shape and sets $container for systemd.
    boot.isContainer = true;

    # The developer owns the box: one login user, wheel + passwordless sudo.
    # This is environment selection, not a security boundary — the whole VM
    # is single-tenant and reachable only as this user over the WG-identity
    # SSH server.
    users.users.${cfg.loginUser} = {
      isNormalUser = true;
      uid = 1000;
      home = "/home/${cfg.loginUser}";
      shell = cfg.loginShell;
      extraGroups = [
        "wheel"
        # The socket dockerSocket.enable creates below is mode 0660 root:podman
        # (the podman module pins SocketGroup = "podman"), so without this the
        # only user who ever logs in cannot open it and the whole point of
        # exposing /run/docker.sock is lost. This concedes nothing: the same
        # user already has passwordless sudo and the VM is single-tenant.
        "podman"
      ];
    };
    security.sudo.wheelNeedsPassword = false;

    # The substrate the agent shells out to (wgnet/identity) + the attach
    # transports. The client layers everything else.
    environment.systemPackages = with pkgs; [
      wireguard-tools
      iproute2
      procps # general process utilities
      git
      curl
    ];
    # Deliberately NOT in the base: a C/C++ toolchain (gcc/gnumake/binutils/
    # pkg-config). `pip install` of a wheel-less package, node-gyp, and
    # cc-based cargo build scripts all need one — but the wrapped gcc alone is
    # ~310 MiB of closure (~50% of the whole base), so it is a client-layer
    # choice: add it in your own extraModules if your workflow compiles native
    # code. (Decided at FIX-323 review; the nix-ld set below still covers
    # PREBUILT binaries, which need no compiler.)

    # Install the terminfo database for every terminal a developer might SSH
    # in from. A devbox doesn't control the client terminal, and modern
    # terminals advertise their own TERM (Ghostty → xterm-ghostty, plus
    # kitty/alacritty/wezterm/foot). With no matching terminfo entry, any
    # program that initializes the screen via its pager — jj/git → less — has
    # less fall back to dumb mode and print "WARNING: terminal is not fully
    # functional / Press RETURN to continue" before every paged command. This
    # installs the .terminfo output of ghostty and the other common terminals
    # (see nixos/modules/config/terminfo.nix), so TERM resolves and the warning
    # goes away.
    environment.enableAllTerminfo = true;

    # Give fontconfig something to find. fonts.fontconfig.enable already
    # defaults to true, so /etc/fonts was being generated all along — but
    # fonts.packages was empty, so it listed no font directories and every
    # lookup came back with nothing. Headless tools that rasterize are the
    # casualties: matplotlib/Pillow chart rendering, HTML-to-PDF converters and
    # headless Chromium screenshots either raise "cannot find font family" or
    # silently render tofu. DejaVu is the family the Python plotting stacks
    # assume by default; Liberation is metric-compatible with
    # Arial/Times/Courier, which is what PDF and screenshot tools request by
    # name. About 14 MiB for the pair. CJK and emoji coverage is a consumer's to
    # add in its own layer — the Noto families for those outweigh this entire
    # font set several times over.
    fonts.packages = with pkgs; [
      dejavu_fonts
      liberation_ttf
    ];

    # nix-ld shim for generic-linux dynamically-linked ELFs. A devbox is a
    # general-purpose developer machine, and the tools developers reach for
    # routinely download prebuilt manylinux binaries that expect the FHS
    # loader (/lib64/ld-linux-x86-64.so.2) NixOS doesn't ship: VS Code's
    # Remote-SSH server, language version managers (pyenv/nvm/rustup), and —
    # the case that surfaced this — Pants, whose scie bootstrap fetches a
    # python-build-standalone CPython and execs it. Without a loader those
    # all die with "Could not start dynamically linked executable"
    # (https://nix.dev/permalink/stub-ld). nix-ld installs the stub loader at
    # the conventional path and points it at this library set. Generic enough
    # to belong in the base (VS Code Remote, language version managers, and
    # prebuilt build tools all need it). Tool-specific FHS /bin shims, when a
    # consumer needs them, belong in that consumer's own extraModules layer,
    # not here.
    #
    # This list is MERGED with the nix-ld module's own default set rather than
    # replacing it — `libraries` is a listOf, and listOf concatenates every
    # definition — so zstd, curl, libxml2, util-linux, systemd, attr, acl,
    # libssh and libsodium already arrive from upstream (as do harmless
    # duplicates of a few entries here that predate this note). Each of the
    # five entries added below (libxcrypt, libsecret, libkrb5, fontconfig,
    # freetype) is absent from the upstream set and names the consumer that
    # fails without it.
    programs.nix-ld = {
      enable = true;
      libraries = with pkgs; [
        stdenv.cc.cc
        zlib
        openssl
        libffi
        sqlite
        xz
        bzip2
        readline
        ncurses
        # libcrypt.so.1. glibc dropped its crypt implementation in 2.28, and
        # NixOS keeps that SONAME only in libxcrypt's obsolete-API build. It is
        # on the manylinux ABI whitelist, so wheels and prebuilt interpreters
        # link it and die at load with "libcrypt.so.1: cannot open shared object
        # file". Already in the system closure via shadow/PAM — costs nothing.
        libxcrypt
        # libsecret-1.so.0. VS Code's Remote-SSH server keeps its tokens in the
        # secret-service client library, and prebuilt CLIs (gh among them)
        # dlopen it for credential storage. Note there is no keyring DAEMON on
        # the box, which is the intended end state: a missing library is a hard
        # loader error that kills the process, whereas a missing service is a
        # clean D-Bus failure the caller already handles by falling back to a
        # plaintext store.
        libsecret
        # libgssapi_krb5.so.2. Remote-SSH's kerberos native module loads it for
        # authenticating proxies, and manylinux database drivers reach it
        # through libpq. Already in the closure via curl's GSSAPI support.
        libkrb5
        # libfontconfig.so.1 / libfreetype.so.6, dlopened by the AWT font
        # manager of any JDK installed outside Nix (SDKMAN, a Gradle or Maven
        # toolchain download) the first time anything touches headless
        # java.awt, and by prebuilt headless renderers generally. Pairs with
        # fonts.packages above: the loader finds the library, fontconfig finds
        # a face.
        fontconfig
        freetype
        # Deliberately absent: the Chromium/Electron GUI stack (nss, nspr, atk,
        # cups, libdrm, gtk3, mesa, the X11 client libraries). An "Electron"
        # language server is a stdio process that needs none of it, while a real
        # headless Chromium needs all of it — several hundred MiB of closure on
        # every box for a tool most never run. That belongs in the consumer's
        # own extraModules layer.
      ];
    };

    # Same scie-bootstrap saga as nix-ld above, one layer up. nix-ld lets the
    # python-build-standalone CPython that Pants's scie fetches *exec*; this
    # lets it — and every other prebuilt manylinux tool a developer reaches for
    # (rustup, uv, node prebuilds, language-server binaries) — find a CA trust
    # store. Those binaries link a non-nixpkgs OpenSSL, which honors the
    # standard SSL_CERT_FILE/SSL_CERT_DIR but NOT the nixpkgs-only
    # NIX_SSL_CERT_FILE patch the rest of the system rides on. Without this they
    # fail TLS verification with "unable to get local issuer certificate" even
    # though curl, git, and the JVM all work — which is exactly how this
    # surfaced (`pants test ::` → scie download → CERTIFICATE_VERIFY_FAILED).
    #
    # Pointing at /etc/ssl/certs/ca-certificates.crt (not the pkgs.cacert store
    # path the scratch OCI images in flake.nix must use) tracks the live system
    # trust store, so a client image that adds CAs via security.pki is honored
    # for free. GIT_SSL_CAINFO covers git subprocesses those tools spawn.
    environment.variables = {
      SSL_CERT_FILE = "/etc/ssl/certs/ca-certificates.crt";
      SSL_CERT_DIR = "/etc/ssl/certs";
      GIT_SSL_CAINFO = "/etc/ssl/certs/ca-certificates.crt";
    };

    # Drop interactive logins into the baked repo working tree, so a fresh
    # box opens on the code rather than an empty $HOME. The path is supplied
    # by the image (cfg.repoDir, set by mkDevimage) — the base never names a
    # directory, so this is agnostic to what the client's repo is called.
    #
    # loginShellInit lands in /etc/profile, which a login shell sources. The
    # agent runs an interactive session as `$SHELL -l` (a login shell) but
    # `ssh box <cmd>` as `$SHELL -c <cmd>` (NOT a login shell — see
    # devboxes-agent sshserver handleSession), so this fires for real
    # interactive logins only and never perturbs exec/scp/sftp. The `case $-`
    # interactive guard is belt-and-suspenders for any other `-l -c` caller;
    # the `$PWD = $HOME` guard keeps a user who already cd'd (or a re-sourced
    # profile) from being yanked back; the `-d` check tolerates a box whose
    # persisted volume predates the baked tree.
    environment.loginShellInit = lib.mkIf (cfg.repoDir != null) ''
      case $- in
        *i*)
          if [ "$PWD" = "$HOME" ] && [ -d ${lib.escapeShellArg cfg.repoDir} ]; then
            cd ${lib.escapeShellArg cfg.repoDir} 2>/dev/null || true
          fi
          ;;
      esac
    '';

    # No public listeners: the agent's SSH server binds wg0's address; wg
    # itself initiates outbound to the relay (conntrack admits replies). The
    # NixOS firewall adds nothing here but boot-time nftables surface.
    networking.firewall.enable = false;

    # No openssh: the agent IS the SSH server (WG-identity).
    services.openssh.enable = lib.mkForce false;

    # rootless podman with the docker CLI alias rides the systemd base.
    virtualisation.podman = {
      enable = true;
      dockerCompat = true;
      # Put podman's socket at /run/docker.sock as well. dockerCompat only
      # installs a `docker` → `podman` command alias, which does nothing for a
      # LIBRARY: testcontainers (every language binding), the Docker SDKs and
      # docker-compose implementations talk to the API socket directly, probing
      # DOCKER_HOST then /var/run/docker.sock (a symlink to /run on NixOS) and
      # failing with "Cannot connect to the Docker daemon" when neither answers.
      # Podman still supports only one socket, so nixpkgs implements this as a
      # symlink to the SYSTEM socket — meaning containers created through it are
      # root-owned and are not the ones `docker ps` (which runs rootless, as the
      # login user) lists. That split is inherent to podman, not to this line.
      dockerSocket.enable = true;
    };

    # Re-assert the nameservers the image entrypoint captured from the
    # platform-seeded /etc/resolv.conf (/etc/devboxes/boot-resolv.conf, written
    # pre-pivot — see mkDevimage's initScript). This is the canonical
    # explanation of FIX-321; the entrypoint and the test point back here.
    #
    # `boot.isContainer` turns on `networking.useHostResolvConf`, so stage-2 runs
    # `resolvconf -m 1000 -a host </etc/resolv.conf` every boot, and that `host`
    # record is what normally carries the platform's DNS into the generated file.
    # On a broken box it is missing or empty and the generated file has NO
    # nameserver line, so nothing on the box can resolve the control plane.
    #
    # Two candidate mechanisms, not distinguished on a live box. Re-asserting
    # AFTER systemd is up is correct under both — early seed-restoration alone
    # (which the entrypoint now also does, see mechanism 1) covers only the
    # first:
    #   1. stage-2 reads /etc/resolv.conf POST-pivot — the overlay. A
    #      nameserver-less file in the persisted upper layer used to make every
    #      later boot re-register that empty file. mkDevimage's entrypoint now
    #      rm's the upper copy pre-overlay on EVERY boot (FIX-323), so this
    #      mechanism self-heals at the source; this unit remains the fallback
    #      for any boot where that rm's Fly-reseeds-every-boot assumption
    #      fails.
    #   2. or stage-2's /run/resolvconf state doesn't survive to
    #      resolvconf.service — i.e. its own registration is what failed, which
    #      restoring the seed earlier does not fix; only this unit covers it.
    #
    # `resolvconf -a`, not an append: the record is REGISTERED, so later
    # regenerations keep it instead of dropping it again. Idempotent.
    #
    # ConditionFileNotEmpty makes a boot that captured nothing a clean skip
    # rather than a failed unit; a genuine `-a` failure is left to fail the unit,
    # since swallowing it would put the silent outage right back. Scoped to
    # systems where resolvconf owns /etc/resolv.conf — a client image that
    # disables it manages that file some other way.
    systemd.services.devboxes-resolv = lib.mkIf config.networking.resolvconf.enable {
      description = "devboxes-resolv — re-assert the boot-captured nameservers";
      # One pull-in and one ordering edge, each stated once: the agent declares
      # Wants=, this declares Before=. No wantedBy — the unit exists for the
      # agent, so it runs exactly when the agent does.
      before = [ "devboxes-agent.service" ];
      # Load-bearing, not mere sequencing: openresolv's libc subscriber exits 1
      # on a signature mismatch for every command except `-u`, so running before
      # resolvconf.service has normalized /etc/resolv.conf — on the first boot it
      # is still the platform's raw seed — would fail this unit outright.
      after = [ "resolvconf.service" ];
      unitConfig.ConditionFileNotEmpty = "/etc/devboxes/boot-resolv.conf";
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        # Before= makes the agent wait on this, so bound the wait explicitly.
        TimeoutStartSec = "20s";
      };
      # `-m 1100` and the NON-`lo` key are one property: openresolv's key_order
      # globs `lo.*` to the head of the list before metrics are read, so an `lo.`
      # key would outrank stage-2's host record (1000) and a client image's own
      # `networking.nameservers` (metric 1). 1100 makes this a fallback that
      # decides only when nothing better registered — the broken boot.
      script = ''
        ${lib.getExe config.networking.resolvconf.package} -a boot.devboxes -m 1100 \
          < /etc/devboxes/boot-resolv.conf
      '';
    };

    systemd.services.devboxes-agent = {
      description = "devboxes-agent — control-plane liaison (wg0, WG-identity SSH, heartbeat, config pull)";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];
      # Pulls in the DNS re-assert, under the same condition that defines it.
      # Wants, not Requires: a box that captured nothing must still get an agent.
      wants = lib.mkIf config.networking.resolvconf.enable [ "devboxes-resolv.service" ];
      path = with pkgs; [
        wireguard-tools
        iproute2
        procps
      ];
      serviceConfig = {
        # The RIFT_* machine env, captured by the image entrypoint before
        # the pidns handoff (systemd never sees Fly's machine env directly).
        EnvironmentFile = "/etc/devboxes/boot-env";
        ExecStart = "${cfg.agentPackage}/bin/devboxes-agent";
        Restart = "always";
        RestartSec = "2s";
      };
      environment = {
        RIFT_STATE_DIR = "/var/lib/devboxes";
        RIFT_LOGIN_SHELL = "${cfg.loginShell}/bin/${cfg.loginShell.meta.mainProgram or "bash"}";
        # The single login user every authorized peer lands as — the session
        # Manager spawns each session's shell as this user. Exported (not left to
        # the agent's "dev" fallback) so an overridden loginUser stays authoritative.
        RIFT_LOGIN_USER = cfg.loginUser;
      };
    };

    system.stateVersion = "24.11";
  };
}
