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
# (mkDevimage's init script), which assembles the overlay root (image = RO
# lower, volume upper at /persist), captures the machine's RIFT_* env to
# /etc/devboxes/boot-env (systemd-in-a-child-pidns does NOT inherit machine
# env), pivots, and execs this system's stage-2 under
# `unshare --pid --fork --mount-proc` — systemd then runs as ns-PID-1
# (getpid()==1 ⇒ real init mode).
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.rift.devboxes-base;

  # devbox-secrets `env`-strategy secrets (e.g. std:claude → CLAUDE_CODE_OAUTH_TOKEN)
  # each land one file under ~/.config/rift/env.d/, named for the variable;
  # export every one as an environment variable in interactive shells. Generic by
  # design: a new env-var secret needs no change here — just an `env` declaration
  # in the CLI's secrets registry. The secrets layer can't
  # write shell-rc files (its deniedDest denylist blocks them), so the trusted image
  # owns the export. The filenames are validated identifiers ([A-Za-z_][A-Za-z0-9_]*)
  # and the dest is tool-derived, never manifest-supplied — so `export "$name=…"` is
  # injection-safe; `-s` follows the tmpfs symlink and skips an absent/dangling/empty
  # value. A variable is identical on every push, so a tmpfs re-push is idempotent.
  envSecretsInit = ''
    if [ -d "$HOME/.config/rift/env.d" ]; then
      for _f in "$HOME"/.config/rift/env.d/*; do
        [ -s "$_f" ] || continue
        export "$(basename "$_f")=$(cat "$_f")"
      done
    fi
  '';
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
      extraGroups = [ "wheel" ];
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

    # Export devbox-secrets `env`-strategy secrets (envSecretsInit, in the `let`
    # above). interactiveShellInit lands in /etc/bashrc, which every interactive
    # shell sources — including the agent's `$SHELL -l` login session (NixOS's
    # /etc/profile sources /etc/bashrc when interactive) and any subshell/tmux pane
    # the user spawns. `$SHELL -c` (exec/scp/sftp) is non-interactive and sources
    # neither, so secrets are never baked into a subprocess env.
    environment.interactiveShellInit = envSecretsInit;

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
    };

    # tmpfs store for pushed secrets. The
    # unprivileged `rift` push can't create a mount, so the image provisions
    # one: a small, hardened, login-user-owned tmpfs. Secret bytes land here
    # (never on the persistent /persist overlay), so they vanish on reboot and
    # are re-pushed on the next connect; a `~/.*` dest is a symlink into this
    # store. systemd.mounts (not fileSystems) because the image skips initrd and
    # execs stage-2 systemd directly — a stage-2 mount unit is the predictable
    # path, and /run is already a systemd tmpfs. noexec/nosuid/nodev harden it;
    # noswap (Linux ≥6.4, which Fly's microVM kernel is) keeps pages off disk —
    # belt-and-suspenders, the image also configures no swap/zram.
    systemd.mounts = [
      {
        what = "tmpfs";
        where = "/run/devbox-secrets";
        type = "tmpfs";
        options = "mode=0700,uid=${
          toString config.users.users.${cfg.loginUser}.uid
        },gid=100,size=32M,noexec,nosuid,nodev,noswap";
        wantedBy = [ "multi-user.target" ];
        before = [ "devboxes-agent.service" ];
      }
    ];

    systemd.services.devboxes-agent = {
      description = "devboxes-agent — control-plane liaison (wg0, WG-identity SSH, heartbeat, config pull)";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];
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
