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
let
  # mkDevimage packs a system built on devboxes-base into the Fly-bootable OCI:
  # the entrypoint runs the overlay-root recipe (image = RO lower, volume upper
  # at /persist; pivot_root; systemd as PID 1 of a child pid namespace — Fly's
  # init keeps VM PID 1), captures the machine's RIFT_* env to
  # /etc/devboxes/boot-env and its seeded nameservers to
  # /etc/devboxes/boot-resolv.conf before the handoff (systemd in the pidns
  # never sees machine env; the capture is belt-and-suspenders DNS — the
  # entrypoint also rm's any stale overlay resolv.conf at its source), and
  # forwards `fly machine stop`'s SIGTERM as SIGRTMIN+3 to
  # ns-systemd for a clean shutdown. TARGETS x86_64-linux (Fly's arch); on an
  # aarch64 host the system closure builds under binfmt emulation.
  #
  # WHAT boots is resolved at RUNTIME via a two-rung ladder (rift image-model
  # §3.5) — the volume profile /persist/nix/var/nix/profiles/system when its
  # toplevel actually resolves under /persist, else the toplevel baked at
  # image build — under a two-stage supervisor (§3.7) that starts the child
  # ONCE, propagates its real exit status to Fly, and on the agent's SIGUSR1
  # recovery request tears the client system down and boots the baked
  # bootstrap in its place. With `bootstrap = true` the image additionally
  # gains the realizer stage (substitute $SYSTEM_PATH onto the volume from
  # the tenant cache and set the profile) and starts the agent OUTSIDE the
  # client system as a sibling of the pidns handoff (INV-5).
  #
  # Hoisted into this `let` (rather than defined inline as `lib.mkDevimage`) so
  # the sibling `lib.mkRift` below can call it by bare name — attrset values
  # don't see each other's keys without `rec`.
  mkDevimage =
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
      # Build the BOOTSTRAP image variant (rift image-model §3.5/§3.7): adds
      # the realizer init stage — substitute $SYSTEM_PATH onto the /persist
      # volume from the tenant cache and set the volume profile — and starts
      # the agent OUTSIDE the client system as a direct child of the image
      # init (INV-5; the baked system also gets
      # rift.internal.agentInSystem = mkDefault false, so the in-system unit
      # and the outside agent can never both run). false (the default) is the
      # classic client/base image: no nix in the closure, no realizer, the
      # agent in-system.
      bootstrap ? false,
      # Extra Nix binary-cache public keys the realizer trusts beside
      # cache.nixos.org's — the platform signing keys (rift image-model
      # §3.12), baked into the init at image build so a pinned bootstrap can
      # verify tenant-cache paths. require-sigs stays true on the box. Only
      # consulted when bootstrap = true.
      cacheTrustedPublicKeys ? [ ],
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
        vendorHash = "sha256-panH7srPU1vA741UxOtoBlM0xGByjfWKFQ8vqRVK1s4=";
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
        # A bootstrap image starts the agent OUTSIDE the client system from
        # its init (INV-5); shipping the in-system unit too would run two
        # agents for one workspace on a recovery boot. bootstrap = true is the
        # sole writer of this today; mkDefault so an extraModules definition
        # could still override cleanly if one ever needs to.
        ++ nixpkgs.lib.optional bootstrap {
          rift.internal.agentInSystem = nixpkgs.lib.mkDefault false;
        }
        ++ extraModules;
      };
      toplevel = baseSystem.config.system.build.toplevel;

      # Config facts of the BAKED system the bootstrap init needs at runtime,
      # resolved from that system's own module eval so they cannot drift from
      # what module.nix's in-system agent unit would have exported.
      baseCfg = baseSystem.config.rift.devboxes-base;

      # The realizer's signature trust set: a substituted path must carry a
      # signature by one of these — require-sigs stays true on the box (rift
      # image-model §3.12). cache.nixos.org's well-known key serves the
      # public paths the builder deliberately does not copy into the tenant
      # prefix; cacheTrustedPublicKeys carries the platform signing keys.
      realizerTrustedKeys = nixpkgs.lib.concatStringsSep " " (
        [ "cache.nixos.org-1:6NCHdD59X431o0gWypbMrAURkbJ16ZPMQFGspcDShjY=" ] ++ cacheTrustedPublicKeys
      );

      # Capture the DNS the platform seeded into the rootfs. Read pre-pivot so
      # the capture holds INDEPENDENTLY of the upper-cleanup rm in the
      # entrypoint (FIX-323): with that rm in place the post-pivot view is
      # Fly's fresh lower copy too, but if the rm — or its assumption that Fly
      # reseeds every boot — ever regresses, the post-pivot view can again be
      # a stale volume copy, and this capture must not inherit that failure.
      # devboxes-resolv re-asserts what this captures; the full FIX-321 story
      # is in its comment in nix/devboxes-base/module.nix.
      #
      # Captured verbatim — a resolvconf record IS a resolv.conf, which is how
      # stage-2 registers the host record (`resolvconf -a host </etc/resolv.conf`).
      # The `nameserver` match at column 0 (what glibc and openresolv parse) is
      # only the decision to overwrite: a boot that yields none keeps the
      # last-known-good capture on the volume instead of clobbering it.
      #
      # Pure bash, so there is no `grep` to resolve from a PATH this script sets
      # itself; `|| [ -n "$line" ]` catches a final line with no trailing newline.
      #
      # `-f` and `|| :` are both load-bearing: errexit is armed inside an `if`
      # body, so a directory here would abort the entrypoint and a FIFO would
      # hang it — before the pivot, i.e. a machine that never boots, which is
      # worse than the unreachable box this fixes. Unlike the `> boot-env` write
      # above, this path comes from Fly's init, not from this script. The `else`
      # branch is what lets `fly logs` answer "did the capture fire?" — without
      # it a boot where this does nothing looks like one that never needed it.
      #
      # Hoisted out of initScript (and exposed via the image's passthru) so the
      # test can run it against fixture resolv.confs — the behaviour, not the
      # spelling, is what must not regress.
      captureBootResolv = ''
        {
          if [ -f /etc/resolv.conf ]; then
            seed="" seed_ns=""
            while IFS= read -r line || [ -n "$line" ]; do
              seed="$seed$line"$'\n'
              case $line in
                nameserver[[:space:]]*) seed_ns=y ;;
              esac
            done < /etc/resolv.conf
          fi
          if [ -n "''${seed_ns:-}" ]; then
            printf '%s' "$seed" > /newroot/etc/devboxes/boot-resolv.conf
          else
            echo "FIX-321: seeded /etc/resolv.conf carried no nameserver; devboxes-resolv will skip"
          fi
        } || :
      '';

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

        # --- Recovery flag: unlink-on-read, at the very top of every boot ---
        # /persist/rift/recovery-inflight is the ONE-SHOT flag the SIGUSR1
        # trap below persists before killing a failed client system (rift
        # image-model §3.7). Consuming it here — unlink immediately on read —
        # scopes its effect to exactly one boot: it forces THIS boot onto
        # ladder rung 2 (the baked bootstrap) and suppresses exit-status
        # propagation for THIS boot only. Left unclearable, a machine that
        # recovery-booted once would permanently lose exit-code propagation —
        # the very mechanism the two-stage supervisor exists to restore.
        mkdir -p /persist/rift
        RECOVERY_BOOT=0
        if [ -e /persist/rift/recovery-inflight ]; then
          rm -f /persist/rift/recovery-inflight
          RECOVERY_BOOT=1
        fi

        mkdir -p /persist/upper /persist/work /lower /newroot

        # /etc/resolv.conf must never survive in the overlay upper. Fly's init
        # writes a fresh one into the image root on every boot, but NixOS
        # stage-2 feeds whatever /etc/resolv.conf it *sees* back into openresolv
        # (`useHostResolvConf`, which boot.isContainer turns on by default —
        # nixos/modules/system/boot/stage-2-init.sh), and openresolv then
        # rewrites the file. That write copies up into /persist/upper, and from
        # the next boot on the upper copy shadows Fly's, so stage-2 reads back
        # its own previous output: a feedback loop with the volume as its
        # memory. dhcpcd never gets a lease on Fly, so the moment the loop
        # latches onto an empty file it stays empty and ALL DNS is dead —
        # permanently, on that volume. That is the observed failure.
        #
        # Dropping the upper entry here, before the overlay is assembled, lets
        # Fly's copy show through the lower again; `rm` also clears a whiteout
        # if some boot deleted the file outright. It runs every boot, so the
        # loop can never latch, and it repairs volumes already poisoned in the
        # field rather than only preventing new ones.
        #
        # The alternative — re-seeding after pivot, i.e. copying the lower's
        # file over the overlay's — is strictly weaker: it repairs the upper
        # only when Fly's copy is present and non-empty, so in exactly the case
        # that produced the outage it writes nothing and leaves the poison in
        # place. Making /etc/resolv.conf a bind-mounted tmpfs file would rule
        # the persistence out structurally, but openresolv installs the file
        # with rename(2), which returns EBUSY onto a bind-mount target — every
        # boot would then fail its resolvconf run loudly.
        #
        # COMPANION, not competitor, to FIX-321's capture + devboxes-resolv
        # re-assert (below and in module.nix): FIX-321 out-competes a bad
        # registration after boot (metric 1100, and it also covers the
        # capture-side failure mode this rm can't reach); this rm removes the
        # poison at its source, so stage-2's ordinary host record (metric
        # 1000) works again, the volume itself heals, and FIX-321's unit is
        # genuinely a fallback rather than the primary DNS path.
        rm -f /persist/upper/etc/resolv.conf

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

        # Capture the machine's RIFT_* env for the agent — systemd in the
        # child pidns does NOT inherit machine env. Root-only: the bearer
        # token (and the cache read credential) live here.
        #
        # The name list is the §3.5 boot-env allowlist (the design's boot-env
        # contract table), stated as a hardcoded loop deliberately: every new
        # value is an explicit edit here. The launch-event producers for the
        # SYSTEM_PATH / RIFT_BK / RIFT_REVOKE_EPOCH / RIFT_CACHE_* /
        # RIFT_REPO_* rows land in the design's PR 7 — until then those are
        # absent from the machine env and written EMPTY, which every consumer
        # treats as unset. Values must stay shell-assignment-safe (no spaces,
        # quotes, or expansions): the file is parsed both by systemd
        # EnvironmentFile= and by bash `.`-sourcing under `set -a` in the
        # agent-start block below.
        umask 077
        # `set +x` around the writer: xtrace would print each iteration's
        # expanded printf — the bearer token and the cache read secret —
        # into `fly logs` (same hazard and same bracketing as the agent-start
        # block below).
        set +x
        {
          for v in RIFT_WORKSPACE_ID RIFT_API_URL RIFT_TOKEN RIFT_WG_IP RIFT_RELAY_ENDPOINT \
            SYSTEM_PATH RIFT_BK RIFT_REVOKE_EPOCH \
            RIFT_CACHE_ENDPOINT RIFT_CACHE_PREFIX RIFT_CACHE_KEY_ID RIFT_CACHE_SECRET \
            RIFT_REPO_URL RIFT_REPO_REF RIFT_REPO_COMMIT RIFT_REPO_DIR; do
            printf '%s=%s\n' "$v" "''${!v:-}"
          done
        } > /newroot/etc/devboxes/boot-env
        set -x

        ${captureBootResolv}

        # Back to a sane umask immediately. The 077 above is scoped to the two
        # /etc/devboxes captures alone — boot-env holds the bearer token and
        # must stay root-only (boot-resolv.conf just rides the same scope).
        # umask survives exec, so leaving it set hands 077 to everything
        # downstream of this script, and the victim is NixOS stage-2: it sets
        # no umask of its own and runs the entire activation script set
        # (setup-etc, the users/groups pass, every directory an activation
        # snippet mkdir's without an explicit chmod) BEFORE it execs systemd.
        # Those all come out 0700/0600, and the single non-root login user this
        # box exists for can then neither traverse nor read them. systemd
        # itself is not at risk — as PID 1 it resets to umask(0) and gives
        # every unit UMask=0022 no matter what it inherited — which is exactly
        # why the leak is easy to miss: the box boots fine and only user-facing
        # permissions are quietly wrong.
        umask 022

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

        # /run/rift — the supervisor's runtime handshake surface: the
        # recovery-boot marker (this boot is a recovery boot), sd-pid (the
        # client system's PID-1 host pid, what sessions nsenter through —
        # rift image-model §3.6), boot-rung (which ladder rung the running
        # child was started on), and the realizer's failure status. Created
        # post-pivot so it lands in the final root's /run regardless of
        # whether Fly gave us a /run tmpfs to rbind above.
        mkdir -p /run/rift
        # Boot-scoped only if /run is a tmpfs: without one these land in the
        # overlay upper and persist to /persist/upper/run/rift across machine
        # boots (same overlay-upper persistence as the resolv.conf rm above),
        # so clear last boot's markers before this boot writes its own.
        rm -f /run/rift/recovery-boot /run/rift/sd-pid /run/rift/realization-status /run/rift/boot-rung
        if [ "$RECOVERY_BOOT" = 1 ]; then
          # Marker: THIS boot is a recovery boot. The agent reads it to
          # report client_system="recovery" and to banner sessions that land
          # in the bootstrap rather than a normal box.
          touch /run/rift/recovery-boot
        fi

        # Rung 2 of the boot ladder: the toplevel baked at image-build time.
        # This interpolation was the ONLY boot path before the ladder existed
        # (which is exactly why a cold restart reverted to bare base — §3.5);
        # it is now the explicit fallback.
        BOOTSTRAP_SYSTEM=${toplevel}

        ${nixpkgs.lib.optionalString bootstrap ''
          # --- Realizer (§3.5): substitute the client system onto the volume.
          # An init STAGE — after pivot_root (so /persist is the volume and
          # the final root is assembled) and BEFORE the boot ladder — not a
          # systemd unit and not an agent subcommand: it must complete before
          # the ladder resolves, and both alternatives run after something
          # already booted. Two no-ops, by design:
          #   (a) $SYSTEM_PATH empty — no system was requested (no launch
          #       producer yet, or a legacy launch);
          #   (b) the profile already resolves to $SYSTEM_PATH — the
          #       cold-restart case: a restart re-runs this init, finds the
          #       profile correct, and boots it needing no network and no
          #       still-valid cache credential (R5's credential-free
          #       survival).
          # On failure or the 10-minute realization budget: the profile stays
          # UNSET — rung 1 then fails its existence check and the box boots
          # the bare bootstrap — and the reason lands in
          # /run/rift/realization-status for the agent to report. It must
          # NEVER silently boot bare base *pretending* to be the client
          # system; the unset profile plus the agent's report ARE that
          # guarantee (§3.5 "Interrupted realization").
          if [ -n "''${SYSTEM_PATH:-}" ]; then
            profile_now=$(readlink -f /persist/nix/var/nix/profiles/system 2>/dev/null || true)
            if [ "$profile_now" = "$SYSTEM_PATH" ] && [ -d "/persist$SYSTEM_PATH" ]; then
              echo "realizer: profile already at $SYSTEM_PATH (cold restart); nothing to do"
            else
              # Substituters: the tenant cache (when the launch env supplied
              # one) ahead of cache.nixos.org, which still serves the public
              # paths the builder deliberately does not copy into the tenant
              # prefix (§3.12). Producer contract (design PR 7):
              # RIFT_CACHE_ENDPOINT is the substituter base URL — an
              # s3://bucket?endpoint=… form keeps its query string — and
              # RIFT_CACHE_PREFIX is the (Context, RepoId) path spliced into
              # its path part.
              subs="https://cache.nixos.org"
              if [ -n "''${RIFT_CACHE_ENDPOINT:-}" ]; then
                tenant="$RIFT_CACHE_ENDPOINT"
                if [ -n "''${RIFT_CACHE_PREFIX:-}" ]; then
                  case $tenant in
                    *\?*) tenant="''${tenant%%\?*}/$RIFT_CACHE_PREFIX?''${tenant#*\?}" ;;
                    *) tenant="$tenant/$RIFT_CACHE_PREFIX" ;;
                  esac
                fi
                subs="$tenant $subs"
              fi

              # Ensure the volume store exists. It is a CHROOT store —
              # local?root=/persist: physical /persist/nix/store, logical
              # /nix/store — which is exactly what lets rung 1 bind it over
              # /nix/store and exec logical store paths unchanged.
              mkdir -p /persist/nix/store /persist/nix/var/nix/profiles /root

              # NIX_REMOTE= is LOAD-BEARING (§3.5 amendment 2026-08-05):
              # boot.isContainer sets NIX_REMOTE=daemon in the system env and
              # there is no nix-daemon here — this runs before any systemd
              # exists — so it is cleared explicitly rather than inherited
              # from whatever the image env carries; every substitution would
              # otherwise hang against a socket that never appears. The cache
              # credential rides the default AWS env-provider chain. The CA
              # bundle points at the store copy because the FHS
              # /etc/ssl/certs materializes only at stage-2 activation, which
              # has not run yet. HOME=/root keeps nix's own state somewhere
              # real. `timeout 600` is the realization budget: a substitution
              # that cannot finish must not hold boot hostage.
              realize_rc=0
              # `set +x` around the invocation: xtrace would print the
              # expanded AWS_SECRET_ACCESS_KEY=… prefix — the cache read
              # credential — into `fly logs` (same hazard and same bracketing
              # as the boot-env writer and the agent-start block).
              set +x
              NIX_REMOTE= HOME=/root \
                NIX_SSL_CERT_FILE=${targetPkgs.cacert}/etc/ssl/certs/ca-bundle.crt \
                AWS_ACCESS_KEY_ID="''${RIFT_CACHE_KEY_ID:-}" \
                AWS_SECRET_ACCESS_KEY="''${RIFT_CACHE_SECRET:-}" \
                timeout 600 ${targetPkgs.nix}/bin/nix-store \
                --store 'local?root=/persist' \
                --option require-sigs true \
                --option substituters "$subs" \
                --option trusted-public-keys '${realizerTrustedKeys}' \
                --realise "$SYSTEM_PATH" || realize_rc=$?
              set -x

              if [ "$realize_rc" -eq 0 ]; then
                # Success ⇒ set the profile — the single fact the ladder
                # trusts: "profile set" ≡ "realization completed".
                if NIX_REMOTE= HOME=/root ${targetPkgs.nix}/bin/nix-env \
                  --store 'local?root=/persist' \
                  --profile /persist/nix/var/nix/profiles/system \
                  --set "$SYSTEM_PATH"; then
                  echo "realizer: profile set to $SYSTEM_PATH"
                else
                  echo "reason=profile-set-failed" > /run/rift/realization-status
                fi
              elif [ "$realize_rc" -eq 124 ]; then
                echo "reason=realization-budget-exceeded" > /run/rift/realization-status
              else
                echo "reason=substitution-failed" > /run/rift/realization-status
              fi
            fi
          fi

          # --- The agent, OUTSIDE the client system (INV-5) ---
          # A direct background child of this supervisor and a SIBLING of the
          # client-system unshare below — NOT inside it — so it survives
          # exactly the failure that used to be invisible (a broken client
          # system, §3.7) and can drive the SIGUSR1 recovery request. The
          # baked system carries rift.internal.agentInSystem = false, so this
          # is the only agent on the box. Environment mirrors module.nix's
          # in-system unit — the boot-env capture (sourced), the state dir,
          # and the login user/shell, resolved from the BAKED system's own
          # config at image build so they cannot drift — plus RIFT_BOOTSTRAP=1
          # (the agent's "I am outside" flag) and a PATH carrying util-linux
          # for the nsenter every session/exec now performs (§3.6) beside the
          # wg/ip/proc tools the in-system unit's path= supplied. `set +x`
          # first: -x would trace the sourced boot-env line by line, printing
          # the bearer token into `fly logs`.
          (
            set +x
            set -a
            . /etc/devboxes/boot-env
            set +a
            export RIFT_BOOTSTRAP=1
            export RIFT_STATE_DIR=/var/lib/devboxes
            export RIFT_LOGIN_USER=${nixpkgs.lib.escapeShellArg baseCfg.loginUser}
            export RIFT_LOGIN_SHELL=${baseCfg.loginShell}/bin/${baseCfg.loginShell.meta.mainProgram or "bash"}
            export PATH=${
              nixpkgs.lib.makeBinPath (
                with targetPkgs;
                [
                  coreutils
                  util-linux
                  procps
                  wireguard-tools
                  iproute2
                ]
              )
            }
            exec ${agentBin}/bin/devboxes-agent
          ) &
          echo "started devboxes-agent outside the client system (pid $!)"
        ''}

        # --- The boot ladder (§3.5): two rungs, each existence-checked ---
        # Rung 1 — the volume profile, iff its toplevel actually resolves in
        # /persist/nix. Rung 2 — $BOOTSTRAP_SYSTEM, baked above. There is
        # deliberately NO $SYSTEM_PATH rung: "the env var is set at launch
        # whether or not realization succeeded, so checking it first would
        # boot a half-realized closure" (§3.5). The profile is the sole
        # source of truth — "profile set" ≡ "realization completed" — and the
        # operator escape hatch is the realizer honoring SYSTEM_PATH (which
        # sets the profile), not a direct boot of it. A recovery boot (flag
        # consumed at the top) forces rung 2: after a teardown the ladder
        # would just re-resolve the same broken system (§3.7).
        BOOT_RUNG=bootstrap
        BOOT_TARGET="$BOOTSTRAP_SYSTEM"
        if [ "$RECOVERY_BOOT" != 1 ] && [ -L /persist/nix/var/nix/profiles/system ]; then
          # readlink -f walks the generation links to the profile's target.
          # A chroot store records the LOGICAL /nix/store/… path (which -f
          # returns unchanged: the image's /nix/store exists, and -f needs
          # only the dirname to resolve); a physical /persist/nix/store/…
          # form is normalized to logical too, in case a nix version records
          # real paths. The existence check is always the PHYSICAL path.
          profile_target=$(readlink -f /persist/nix/var/nix/profiles/system 2>/dev/null || true)
          case $profile_target in
            /persist/nix/store/*) profile_target="/nix/store/''${profile_target#/persist/nix/store/}" ;;
          esac
          case $profile_target in
            /nix/store/*)
              if [ -d "/persist$profile_target" ]; then
                BOOT_RUNG=profile
                BOOT_TARGET="$profile_target"
              fi
              ;;
          esac
        fi

        # --- Two-stage supervisor (§3.7) ---
        # Handoff: systemd as PID 1 of a child pid namespace — a directly-
        # exec'd systemd sees getpid()!=1 under Fly's init and bails, so run
        # it under a fresh pidns. --kill-child ties systemd's life to this
        # supervisor; the TERM/INT trap forwards Fly's stop signal as
        # SIGRTMIN+3 (halt.target) for a clean unit shutdown.
        #
        # Rung 1 boots the volume-resident toplevel by binding the volume
        # store over /nix/store INSIDE the child's own mount namespace
        # (--mount-proc implies --mount, and the early --make-rprivate keeps
        # the bind child-only) before exec'ing its init. The bash doing the
        # bind is a store-path binary already mapped into memory, so it
        # survives the image store vanishing under it; this supervisor and
        # the agent keep the image-store view. Rung 2 execs the baked
        # bootstrap directly — exactly the pre-ladder handoff.
        UNSHARE_PID=""
        start_system() {
          if [ "$1" = profile ]; then
            chroot . ${targetPkgs.util-linux}/bin/unshare \
              --pid --fork --mount-proc --kill-child \
              ${targetPkgs.bash}/bin/bash -c \
              "${targetPkgs.util-linux}/bin/mount --bind /persist/nix/store /nix/store && exec $2/init" &
          else
            chroot . ${targetPkgs.util-linux}/bin/unshare \
              --pid --fork --mount-proc --kill-child "$2"/init &
          fi
          UNSHARE_PID=$!
          # Publish the rung this child was started on — the literal rung
          # name, `profile` or `bootstrap`. $1, NOT $BOOT_RUNG: the recovery
          # second stage starts `bootstrap` while BOOT_RUNG still holds what
          # the ladder first chose. The agent reads it to arm its
          # health-deadline timeoutAction only when the boot took rung
          # `profile` — a bare-bootstrap boot has no client system to recover.
          printf '%s\n' "$1" > /run/rift/boot-rung
          # Publish the client system's PID-1 host pid (§3.6): the handoff
          # file the agent's sessions nsenter through, replacing per-session
          # pgrep re-derivation. Same retry the shutdown forwarder always
          # used — the fork inside unshare is not instantaneous.
          local sd=""
          for _ in 1 2 3 4 5; do
            sd=$(pgrep -P "$UNSHARE_PID" | head -n1 || true)
            [ -n "$sd" ] && break
            sleep 0.2
          done
          if [ -n "$sd" ]; then
            printf '%s\n' "$sd" > /run/rift/sd-pid
          else
            : > /run/rift/sd-pid
          fi
        }

        forward_shutdown() {
          local sd=""
          if [ -s /run/rift/sd-pid ]; then
            sd=$(cat /run/rift/sd-pid)
          fi
          if [ -z "$sd" ]; then
            sd=$(pgrep -P "$UNSHARE_PID" | head -n1 || true)
          fi
          [ -n "$sd" ] && kill -RTMIN+3 "$sd" || true
        }

        request_recovery() {
          # SIGUSR1 = the agent's recovery request (§3.7): the client system
          # failed its health deadline. Persist the one-shot flag FIRST —
          # crash-safe ordering: if this supervisor dies before the in-place
          # recovery below runs, the next machine boot consumes the flag and
          # recovery-boots anyway. Then take the client system down: a clean
          # halt via the forwarded SIGRTMIN+3, a FIXED 10 s grace, then TERM
          # to the unshare (--kill-child SIGKILLs the pidns on its way out).
          # The grace is fixed, not early-exiting: this handler runs inside
          # wait_child's interrupted `wait`, so an exited child stays a
          # zombie of this shell until that wait resumes and reaps it —
          # kill -0 keeps succeeding on the zombie, the early return below
          # never fires, and the loop always runs its full 10 s. That is
          # acceptable (recovery is already a slow path); the trailing TERM
          # is then a harmless no-op on an already-dead child.
          touch /persist/rift/recovery-inflight
          forward_shutdown
          for _ in $(seq 1 20); do
            kill -0 "$UNSHARE_PID" 2>/dev/null || return 0
            sleep 0.5
          done
          kill -TERM "$UNSHARE_PID" 2>/dev/null || true
        }

        # Single-attempt supervision (§3.7): the child is started ONCE per
        # stage and its REAL exit status captured — the old
        # `while kill -0 …; wait || true` swallowed it, so a failed client
        # system exited 0 and Fly's on-failure policy never fired. The loop
        # is NOT a restart loop: it re-waits only when a trap interrupted the
        # wait (bash returns >128 from `wait` on a caught signal) while the
        # child is still alive; the final re-wait covers the trap firing in
        # the same instant the child died — bash then still remembers the
        # real status, and a wait on an already-reaped child is idempotent.
        wait_child() {
          status=0
          while :; do
            if wait "$UNSHARE_PID"; then status=0; else status=$?; fi
            kill -0 "$UNSHARE_PID" 2>/dev/null || break
          done
          if [ "$status" -gt 128 ]; then
            if wait "$UNSHARE_PID"; then status=0; else status=$?; fi
          fi
          # The published PID-1 went down with its system.
          : > /run/rift/sd-pid
        }

        trap forward_shutdown TERM INT
        trap request_recovery USR1

        start_system "$BOOT_RUNG" "$BOOT_TARGET"
        wait_child

        if [ "$RECOVERY_BOOT" = 1 ]; then
          # This whole boot was a recovery boot (flag consumed at the top):
          # exit-status propagation is suppressed for exactly this boot, so
          # the machine parks cleanly instead of handing Fly's restart policy
          # a recovery loop.
          exit 0
        fi

        if [ -e /persist/rift/recovery-inflight ]; then
          # The agent requested recovery THIS boot (SIGUSR1 above): in-place
          # recovery — boot the baked bootstrap as a second, FINAL child so
          # the box stays reachable for diagnosis. The flag stays on the
          # volume: it suppresses exit propagation now, and the NEXT machine
          # boot's unlink-on-read both restores propagation and forces that
          # boot onto rung 2.
          touch /run/rift/recovery-boot
          start_system bootstrap "$BOOTSTRAP_SYSTEM"
          wait_child
          exit 0
        fi

        # §3.7's fix for the swallowed exit status: a failed client system
        # now exits non-zero, so Fly's on-failure restart policy fires and
        # `fly machine exec` has something to report.
        exit "$status"
      '';
      hostPkgs = import nixpkgs { system = hostSystem; };
      # The registration dump loaded into the image's Nix database (see
      # includeNixDB below). initScript is the right — and only — root: the
      # store content this image ships is exactly its closure, because
      # streamLayeredImage derives the shipped paths from the config JSON,
      # which references nothing but `Entrypoint = [ initScript ]`. (The
      # customisation layer's db.sqlite does textually embed every closure
      # path once this change lands — but those refs ARE initScript's closure,
      # so the root above stays complete.)
      imageRegistration = hostPkgs.closureInfo { rootPaths = [ initScript ]; };
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
      # Ship a POPULATED /nix/var/nix/db. Without a database the ~600 store
      # paths this image carries do not exist as far as Nix is concerned:
      # nothing can be substituted against them, nothing can be realized, and
      # no toplevel can be made a profile generation inside the box.
      #
      # `includeNixDB` alone is not enough here, and the shortfall is silent.
      # nixpkgs builds the database from `closureInfo { rootPaths = contents; }`
      # (pkgs/build-support/docker/default.nix, `mkDbExtraCommand`) — and this
      # call site passes no `contents` at all; the system closure rides in
      # through the config's Entrypoint reference instead. With `contents = [ ]`
      # the registration is an empty file and the db.sqlite that lands in the
      # image has ZERO rows in ValidPaths (verified by building such an image
      # and querying the table). Moving the closure into `contents` is not an
      # option either: streamLayeredImage symlinkJoins `contents` into the image
      # root, which would splat the NixOS toplevel's etc/init/sw over /.
      #
      # So keep includeNixDB for the scaffolding it does build correctly — the
      # database schema, the gcroots directories, the registrationTime reset
      # that keeps the layer reproducible — and append a second load pass over
      # the closure the image actually ships. nixpkgs PREPENDS its block to
      # extraCommands, `nix-store --load-db` is additive, and both run in one
      # shell, so NIX_REMOTE and USER are already exported by the time this
      # runs.
      includeNixDB = true;
      extraCommands = ''
        ${hostPkgs.lib.getExe' hostPkgs.buildPackages.nix "nix-store"} --load-db < ${imageRegistration}/registration
        ${hostPkgs.lib.getExe hostPkgs.buildPackages.sqlite} nix/var/nix/db/db.sqlite \
          "UPDATE ValidPaths SET registrationTime = ''${SOURCE_DATE_EPOCH}"
      '';
      # The initScript's closure (and through it the whole NixOS system
      # toplevel) rides in via the config reference; no explicit contents.
      #
      # When repoSrc is set, bake it into the dev user's home at
      # /home/dev/<repoDirName>. repoSrc is a ready-to-use tree; a caller that
      # passes a depth-1 clone WITH its .git (origin set, the built commit on a
      # tracked branch, shallow boundary set) gets every bit of git setup done
      # before `builtins.path` ingests it. The box carries no forge credential
      # of its own, so pushing from a box is the owner's to wire up — pick an
      # origin URL accordingly. Here we only place the tree and fix
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
      # The entrypoint and the capture snippet ride along so the test can run
      # the snippet against fixture resolv.confs and check it is still spliced
      # into the entrypoint. Both are eval-time reads — `.text` on a writeScript
      # is an ordinary attribute, no build and no import-from-derivation — and
      # the capture is the load-bearing half of FIX-321, whose loss makes
      # devboxes-resolv a silent skip rather than a visible failure.
      #
      # baseSystem is a TEST SEAM (eval-only, like the other two): the
      # bootstrap structure test (oss/cli/cmd/rift/bootstrap_eval_test.go,
      # FIX-324 T17c) reads `.baseSystem.config.systemd.units` to prove the
      # real `bootstrap = true ⇒ rift.internal.agentInSystem = false` wiring
      # removes the in-system devboxes-agent unit from the BAKED system (and
      # that the default keeps it) — a second agent inside the client system
      # would race the outside one on every recovery boot (INV-5). Nothing at
      # runtime reads it.
      passthru = {
        inherit initScript captureBootResolv baseSystem;
      };
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
  #
  # Both hashes below cover a set PRUNED to what the source actually imports, so
  # DELETING Go code can change them even when go.mod/go.sum are untouched —
  # dropping the last importer of a package drops that package from the vendor
  # tree. FIX-233 hit exactly this: deleting sessions/agentforward.go (the only
  # importer of x/crypto/ssh/agent) restaled both hashes with no go.mod diff.
  # Worse, a local `nix build` will not catch it: a fixed-output derivation is
  # reused whenever a store path matching its DECLARED hash already exists, so a
  # machine that built the pre-deletion vendor set keeps silently reusing it and
  # goes green while CI (cold store) fails. To re-derive a hash honestly, set it
  # to lib.fakeHash, build, and take the reported "got:".
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
        vendorHash = "sha256-dRnejPH7Uf4PpKQxvThfKZWP2P/nagVKSCPNdcxhM7E=";
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
        vendorHash = "sha256-panH7srPU1vA741UxOtoBlM0xGByjfWKFQ8vqRVK1s4=";
        ldflags = [
          "-s"
          "-w"
        ];
      };
    };
in
{
  # The devboxes-base substrate module — what a client repo imports to build
  # its own devbox image; `mkDevimage` is the packer that turns a system built
  # on it into the Fly-bootable OCI.
  nixosModules.devboxes-base = ./nix/devboxes-base/module.nix;

  # The image packer a client calls to build its own devbox image (the
  # low-level primitive). Its argument pattern is CLOSED — it rejects unknown
  # args, which is the validation path mkRift delegates to.
  lib.mkDevimage = mkDevimage;

  # mkRift — the versioned `fixed-labs.rift` contract helper (the shape the
  # managed builder reads). It wraps mkDevimage: produces the `{ version = 1;
  # image = … }` envelope so a consumer never writes the version, and defaults
  # `repoSrc = self` / `imageCommit = self.rev or null` so a consumer's checkout
  # is baked without restating it. Open pattern (`...@args`) — every non-`self`
  # arg is forwarded to mkDevimage, whose closed pattern rejects typos.
  #
  # Two subtleties in the body:
  #   - `removeAttrs args [ "self" ]` is REQUIRED: mkDevimage has no `self`
  #     parameter, and its closed pattern would reject it.
  #   - re-injecting `// { inherit repoSrc imageCommit; }` is REQUIRED: Nix's
  #     `@args` binds only the *passed* attrs, NOT the defaulted ones, so
  #     dropping this would send mkDevimage no repoSrc (its own default is null)
  #     and silently bake nothing.
  lib.mkRift =
    {
      self,
      repoSrc ? self,
      imageCommit ? (self.rev or null),
      ...
    }@args:
    {
      version = 1;
      image = mkDevimage ((removeAttrs args [ "self" ]) // { inherit repoSrc imageCommit; });
    };

  inherit packagesFor;
}
