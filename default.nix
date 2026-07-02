# The Nix library — the single source of truth for the base image module, the
# `mkDevimage` image builder, and the two Go derivations (the `rift` CLI and
# the in-box agent).
#
# A plain function of `{ nixpkgs }` — no `system` argument — so a consumer can
# `import ./oss { inherit nixpkgs; }` and inject its own nixpkgs pin. The
# standalone `flake.nix` in this directory wraps it for direct `nix build`.
#
# Returns three things, split by whether they depend on the build system:
#   - nixosModules.devboxes-base : the system-free base module reference.
#   - lib.mkDevimage             : packs a system built on the base into a
#                                  Fly-bootable OCI image (system chosen by the
#                                  hostSystem arg; content is always x86_64).
#   - packagesFor <system>       : the `rift` + `agent` derivations for a system.
{ nixpkgs }:
{
  # The devboxes-base substrate module — what a client repo imports to build
  # its own devbox image; `mkDevimage` is the packer that turns a system built
  # on it into the Fly-bootable OCI.
  nixosModules.devboxes-base = ./nix/devboxes-base/module.nix;

  # mkDevimage packs a system built on devboxes-base into the Fly-bootable OCI:
  # the entrypoint runs the overlay-root recipe (image = RO lower, volume upper
  # at /persist; pivot_root; systemd as PID 1 of a child pid namespace — Fly's
  # init keeps VM PID 1), captures the machine's RIFT_* env to
  # /etc/devboxes/boot-env before the handoff (systemd in the pidns never sees
  # machine env), and forwards `fly machine stop`'s SIGTERM as SIGRTMIN+3 to
  # ns-systemd for a clean shutdown. TARGETS x86_64-linux (Fly's arch); on an
  # aarch64 host the system closure builds under binfmt emulation.
  lib.mkDevimage =
    {
      extraModules ? [ ],
      # An optional repo checkout to bake into the login user's home so a
      # fresh box opens with the code already present and `git pull`/`git
      # fetch` working. A directory path (typically a depth-1 clone WITH its
      # .git). null = bake nothing (a pure, reproducible image — the default).
      repoSrc ? null,
      # The git commit this image carries, recorded so the agent can report
      # it as the resolved commit when it reports the workspace is ready.
      # Normally the HEAD of repoSrc; null = record nothing.
      imageCommit ? null,
      name ? "devimage",
      tag ? "latest",
      # Directory name the baked repo lands under in the login user's home
      # (/home/dev/<repoDirName>). A client repo with a different name passes
      # its own; the login auto-cd follows whatever this is (devboxes-base
      # reads the resulting path, not the name).
      repoDirName ? "repo",
      # The PACKAGING host (dockerTools runs there); image CONTENT is always
      # x86_64 (Fly's arch). Defaults to x86_64 — the canonical builder — so
      # `mkDevimage { }` works on stock CI. An aarch64 host passes
      # hostSystem = "aarch64-linux" explicitly (and needs
      # `extra-platforms = x86_64-linux` + binfmt in the daemon config to build
      # the x86 system closure).
      hostSystem ? "x86_64-linux",
    }:
    let
      targetSystem = "x86_64-linux";
      targetPkgs = import nixpkgs { system = targetSystem; };
      # Where the repo lands (devboxes-base's login user is "dev"). Used both
      # by the fakeRoot bake below and the base's auto-cd option, so the
      # placement and the cd target can't drift.
      repoDest = "/home/dev/${repoDirName}";
      # The agent, built for the image's arch. Same vendorHash as the
      # standalone `agent` output (the go-modules FOD is arch-independent).
      agentBin = targetPkgs.buildGoModule {
        pname = "devboxes-agent";
        version = "0.1.0";
        src = ./agent;
        vendorHash = "sha256-6UlBE+Eqy+n2OG65aZkBiqS9E0h+dc9oXkz21bTBAYE=";
        subPackages = [ "cmd/devboxes-agent" ];
        ldflags = [
          "-s"
          "-w"
        ];
      };
      baseSystem = nixpkgs.lib.nixosSystem {
        system = targetSystem;
        modules = [
          ./nix/devboxes-base/module.nix
          {
            rift.devboxes-base.agentPackage = agentBin;
            # Auto-cd interactive logins into the baked tree; null when no repo
            # is baked (repoSrc = null) so an empty box stays in $HOME. Same
            # path the fakeRoot bake uses (repoDest).
            rift.devboxes-base.repoDir = if repoSrc != null then repoDest else null;
          }
        ]
        # Record the baked commit as a NixOS-managed /etc entry. The agent
        # reads /etc/devboxes/image-commit (config.ImageCommit); it sits beside
        # the runtime-written /etc/devboxes/boot-env, which setup-etc leaves
        # untouched as an unmanaged file (the same coexistence the boot path
        # already relies on).
        ++ nixpkgs.lib.optional (imageCommit != null) {
          environment.etc."devboxes/image-commit".text = imageCommit;
        }
        ++ extraModules;
      };
      toplevel = baseSystem.config.system.build.toplevel;
      initScript = targetPkgs.writeScript "devimage-init" ''
        #!${targetPkgs.bash}/bin/bash
        # Fly runs this as the machine's main process (under Fly's init,
        # which stays VM PID 1). Trace to the console for `fly logs`.
        set -eux
        export PATH=${
          nixpkgs.lib.makeBinPath (
            with targetPkgs;
            [
              coreutils
              util-linux
              procps
            ]
          )
        }

        # The volume must already be mounted by Fly's init.
        mountpoint -q /persist

        mkdir -p /persist/upper /persist/work /lower /newroot
        mount --make-rprivate / || true
        # Non-recursive bind: the lower view excludes the volume + API mounts.
        mount --bind / /lower
        mount -t overlay overlay \
          -o lowerdir=/lower,upperdir=/persist/upper,workdir=/persist/work \
          /newroot

        mkdir -p /newroot/persist /newroot/oldroot /newroot/proc /newroot/dev \
          /newroot/sys /newroot/run /newroot/tmp /newroot/etc/devboxes \
          /newroot/var/lib/devboxes
        chmod 1777 /newroot/tmp
        mount --bind /persist /newroot/persist

        # Capture the machine's RIFT_* env for the agent unit — systemd
        # in the child pidns does NOT inherit machine env. Root-only: the
        # bearer token lives here.
        umask 077
        {
          for v in RIFT_WORKSPACE_ID RIFT_API_URL RIFT_TOKEN RIFT_WG_IP RIFT_RELAY_ENDPOINT; do
            printf '%s=%s\n' "$v" "''${!v:-}"
          done
        } > /newroot/etc/devboxes/boot-env

        # Carry the API filesystems over (NOT /proc — the pidns child gets
        # a namespace-correct one from --mount-proc).
        for m in dev sys run; do
          if mountpoint -q "/$m"; then
            mount --rbind "/$m" "/newroot/$m" || true
            umount -l "/$m" || true
          fi
        done

        cd /newroot
        pivot_root . oldroot
        umount -l /oldroot || true
        # Fresh /proc for THIS (parent) process: pgrep/kill below need it,
        # and the old root's proc left with /oldroot.
        mount -t proc proc /proc

        # cgroup v2 for stage-2 systemd. systemd 258 (NixOS 26.05) REFUSES
        # to boot on a legacy cgroup-v1 hierarchy ("Detected unsupported
        # legacy cgroup hierarchy, refusing execution. Exiting PID 1..." ⇒
        # PID 1 exits 0, the machine stops 2s after start). Fly's init leaves
        # cgroup v1 mounted at /sys/fs/cgroup, which the `--rbind /sys` above
        # carries into our root; and because we exec the NixOS toplevel
        # (stage 2) directly, we skip the initrd that would otherwise mount
        # the unified v2 hierarchy. So mount it here, before the handoff.
        # `--make-rprivate /` above keeps this from disturbing Fly's init.
        umount -R /sys/fs/cgroup 2>/dev/null || true
        mount -t cgroup2 cgroup2 /sys/fs/cgroup

        # Handoff: systemd as PID 1 of a child pid namespace — a directly-
        # exec'd systemd sees getpid()!=1 under Fly's init and bails, so run
        # it under a fresh pidns. --kill-child ties systemd's life to this
        # supervisor; the trap forwards Fly's stop signal as SIGRTMIN+3
        # (halt.target) for a clean unit shutdown.
        chroot . ${targetPkgs.util-linux}/bin/unshare \
          --pid --fork --mount-proc --kill-child ${toplevel}/init &
        UNSHARE_PID=$!

        forward_shutdown() {
          local sd=""
          for _ in 1 2 3 4 5; do
            sd=$(pgrep -P "$UNSHARE_PID" | head -n1 || true)
            [ -n "$sd" ] && break
            sleep 0.2
          done
          [ -n "$sd" ] && kill -RTMIN+3 "$sd" || true
        }
        trap forward_shutdown TERM INT

        while kill -0 "$UNSHARE_PID" 2>/dev/null; do
          wait "$UNSHARE_PID" || true
        done
      '';
      hostPkgs = import nixpkgs { system = hostSystem; };
    in
    hostPkgs.dockerTools.streamLayeredImage {
      inherit name tag;
      architecture = "amd64";
      # Spread the closure across more layers than the nixpkgs default of
      # 100. This closure is ~530 store paths, so at the default the ~430
      # paths that didn't earn their own layer collapsed into ONE giant
      # catch-all layer, whose multi-minute chunked upload kept 502'ing the
      # Fly registry (it outlived Fly's backend timeout). 120 sits just under
      # the OCI 127-layer ceiling (streamLayeredImage spends one on the
      # customization layer), peeling the biggest tail paths into their own
      # blobs so no single layer dominates the push.
      maxLayers = 120;
      # The initScript's closure (and through it the whole NixOS system
      # toplevel) rides in via the config reference; no explicit contents.
      #
      # When repoSrc is set, bake it into the dev user's home at
      # /home/dev/<repoDirName>. repoSrc is a ready-to-use depth-1 clone WITH
      # its .git (origin pinned to the canonical ssh URL so the box's git
      # pull/push rides the forwarded ssh-agent, the built commit on a tracked
      # branch, shallow boundary set) — every bit of git setup happens before
      # `builtins.path` ingests it, so here we only place the tree and fix
      # ownership/perms. It lands in the image's RO lower; the overlay-root
      # boot makes it writable and edits persist to the /persist volume (like
      # any other path). Runs under fakeroot so the chown to the dev user
      # (uid 1000 / gid 100 = "users", per devboxes-base) sticks; chmod u+w
      # restores write bits the read-only Nix store strips, so the box owner
      # can actually edit the tree.
      fakeRootCommands = nixpkgs.lib.optionalString (repoSrc != null) ''
        mkdir -p home/dev
        cp -a --no-preserve=ownership ${repoSrc} home/dev/${repoDirName}
        chown -R 1000:100 home/dev
        chmod -R u+w home/dev/${repoDirName}
        chmod 0700 home/dev
      '';
      config = {
        Entrypoint = [ "${initScript}" ];
        # The devboxes-base marker. ADVISORY, not a security control: it's a
        # plain OCI config label anyone can set, and the ingest endpoint does
        # NOT verify it (it trusts the per-repo HMAC + the registry-namespace/
        # digest-pin constraint; a registration-time skopeo check remains
        # deferred). Its real job is catching accidental misconfiguration: a
        # build pipeline can fail early when a pushed image lacks the label,
        # i.e. wasn't built on devboxes-base and can't boot the overlay root.
        Labels = {
          "dev.rift.devboxes-base" = "v1";
        };
      };
    };

  # The standalone Go derivations, per build system. `rift` is the developer
  # CLI (cmd/rift → binary `rift`); `agent` is the in-box control-plane liaison
  # (cmd/devboxes-agent → binary `devboxes-agent`, the name the base module's
  # systemd unit runs). Both are pure-Go; the vendorHash hashes the vendored
  # dependency sources, so it is independent of the module's own import path.
  packagesFor =
    system:
    let
      pkgs = import nixpkgs { inherit system; };
    in
    {
      rift = pkgs.buildGoModule {
        pname = "rift";
        version = "0.1.0";
        src = ./cli;
        subPackages = [ "cmd/rift" ];
        vendorHash = "sha256-tj3ld/M4T1Hwcdj5v7H6uLTS7dWA7/99L1mEI9UZmWw=";
        # CGO off — the Go deps (wireguard-go netstack, x/crypto) are pure Go,
        # so the inner binary stays static (small closure, no glibc dep).
        env.CGO_ENABLED = "0";
        ldflags = [
          "-s"
          "-w"
        ];
      };
      agent = pkgs.buildGoModule {
        pname = "agent";
        version = "0.1.0";
        src = ./agent;
        subPackages = [ "cmd/devboxes-agent" ];
        vendorHash = "sha256-6UlBE+Eqy+n2OG65aZkBiqS9E0h+dc9oXkz21bTBAYE=";
        ldflags = [
          "-s"
          "-w"
        ];
      };
    };
}
