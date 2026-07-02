package secrets

import (
	"fmt"
	"strings"
)

// secretsStore is the tmpfs store the base image provisions (mode 0700, owned by
// the login user). tmpfs dests are symlinks into it. A var (not const) so the
// bash-level script tests can point it at a writable temp dir.
var secretsStore = "/run/devbox-secrets"

// pathPrefix pins PATH to the NixOS system profile so coreutils (sha256sum,
// install, base64, mktemp, ln, mv) resolve regardless of the agent's exec PATH
// (an `ssh exec` runs `shell -c`, which sources no profile).
const pathPrefix = "export PATH=/run/current-system/sw/bin:$PATH\n"

// repoDirName derives the box checkout dir under $HOME from the repo id's last
// segment, validating it so it embeds safely in a shell script.
func repoDirName(repoID string) (string, error) {
	_, bare := normalizeRepoID(repoID)
	segs := strings.Split(bare, "/")
	name := segs[len(segs)-1]
	if name == "" || name == "." || name == ".." || !nameCharset(name) {
		return "", fmt.Errorf("cannot derive a safe repo dir name from %q", repoID)
	}
	return name, nil
}

func nameCharset(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// storeName maps a key to its tmpfs store filename ([a-z0-9-] only).
func storeName(k Key) string {
	repl := func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}
	return strings.Map(repl, strings.ToLower(string(k.NS))+"-"+strings.ToLower(k.Name))
}

// readConfigScript reports tmpfs-store presence and base64-emits the repo
// manifest. repoDir is the lowercased repo name (charset-validated → safe to
// embed). The image may bake the checkout dir case-PRESERVED (e.g. acme/MyApp →
// /home/dev/MyApp) while our id is lowercased, so after the exact path misses we
// fall back to the one $HOME child whose name matches case-insensitively — still
// scoped to THIS repo (not the wrong checkout / a planted manifest).
func readConfigScript(repoDir string) string {
	return pathPrefix + `
if [ -d ` + secretsStore + ` ] && [ -w ` + secretsStore + ` ]; then echo "STORE 1"; else echo "STORE 0"; fi
want=` + repoDir + `
f="$HOME/$want/.rift/secrets.json"
if [ ! -f "$f" ]; then
  for d in "$HOME"/*/; do
    b=$(basename "$d" | tr 'A-Z' 'a-z')
    if [ "$b" = "$want" ] && [ -f "$d.rift/secrets.json" ]; then f="$d.rift/secrets.json"; break; fi
  done
fi
if [ -f "$f" ]; then printf 'CONFIG '; base64 -w0 < "$f"; echo; fi
`
}

// readHashesScript reads home-relative dest paths from stdin (one per line) and
// emits "<rel>\t<sha256|->\t<loc>" — the content hash of the live file
// (following the symlink), or "-" when absent / a dangling symlink; loc is 1
// when the dest is a symlink INTO the tmpfs store (so reconcile can force a
// re-push to migrate a persistent secret onto the store after an image upgrade).
func readHashesScript() string {
	return pathPrefix + `
while IFS= read -r rel; do
  [ -z "$rel" ] && continue
  p="$HOME/$rel"
  loc=0
  if [ -L "$p" ]; then
    case "$(readlink "$p" 2>/dev/null)" in ` + secretsStore + `/*) loc=1 ;; esac
  fi
  if [ -e "$p" ]; then
    h=$(sha256sum "$p" 2>/dev/null | cut -c1-64)
    printf '%s\t%s\t%s\n' "$rel" "${h:--}" "$loc"
  else
    printf '%s\t-\t%s\n' "$rel" "$loc"
  fi
done
`
}

// pushScript writes the secret (read from stdin) to the dest, atomically
// (mktemp → chmod → mv). rel/mode/store are charset-validated → embedded
// directly. When tmpfs, the bytes land in the store and dest becomes a symlink;
// the caller has already confirmed the store mount exists.
func pushScript(rel, mode string, tmpfs bool, store string) string {
	s := pathPrefix + "set -e\numask 077\n" +
		`dest="$HOME/` + rel + `"` + "\n" +
		// On-box confinement: refuse if the dest's PARENT directory resolves
		// (through any symlinked component planted by box code) outside $HOME.
		// We resolve the parent, not the dest itself, because a tmpfs dest is a
		// symlink into the store (outside $HOME by design) — resolving the dest
		// would refuse every legitimate re-push (a rotated secret). realpath -m
		// handles a not-yet-existing parent.
		`case "$(realpath -m "$(dirname "$dest")")/" in "$HOME"/*) : ;; *) echo "rift: refusing dest resolving outside home: $dest" >&2; exit 4 ;; esac` + "\n" +
		`mkdir -p "$(dirname "$dest")"` + "\n"
	target := secretsStore + "/" + store
	if tmpfs {
		s += `tmp=$(mktemp ` + secretsStore + `/.tmp.XXXXXX)` + "\n" +
			`trap 'rm -f "$tmp"' EXIT` + "\n" + // clean up the temp on any exit (incl. a failed retry)
			`cat > "$tmp"` + "\n" +
			`chmod ` + mode + ` "$tmp"` + "\n" +
			`mv -f "$tmp" ` + target + "\n" +
			`ln -sfn ` + target + ` "$dest"` + "\n"
	} else {
		s += `d=$(dirname "$dest")` + "\n" +
			`tmp=$(mktemp "$d/.devbox.XXXXXX")` + "\n" +
			`trap 'rm -f "$tmp"' EXIT` + "\n" +
			`cat > "$tmp"` + "\n" +
			`chmod ` + mode + ` "$tmp"` + "\n" +
			`mv -f "$tmp" "$dest"` + "\n"
		if store != "" {
			// Migrating off the store (tmpfs:false / std→persistent): drop the
			// prior tmpfs copy so plaintext doesn't linger in the store.
			s += `rm -f ` + target + ` 2>/dev/null || true` + "\n"
		}
	}
	return s
}
