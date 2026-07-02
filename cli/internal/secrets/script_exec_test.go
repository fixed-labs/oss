package secrets

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// runPushScript runs the generated pushScript under real bash (the box runs it
// as `bash -c <script>`), with HOME set and the secret on stdin. Returns the
// exit code and stderr.
func runPushScript(t *testing.T, home, rel, mode string, tmpfs bool, store, secret string) (int, string) {
	t.Helper()
	cmd := exec.Command("bash", "-c", pushScript(rel, mode, tmpfs, store))
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = strings.NewReader(secret)
	var errb strings.Builder
	cmd.Stderr = &errb
	err := cmd.Run()
	if err == nil {
		return 0, errb.String()
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), errb.String()
	}
	t.Fatalf("running pushScript: %v", err)
	return -1, ""
}

func readVia(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// TestReadConfigScriptCaseInsensitiveDir: the image may bake the checkout dir
// case-preserved (acme/MyApp → /home/dev/MyApp) while our repo id is lowercased,
// so the script must still find the manifest case-insensitively.
func TestReadConfigScriptCaseInsensitiveDir(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs bash + coreutils")
	}
	for _, bin := range []string{"bash", "base64", "tr", "basename"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("missing %s", bin)
		}
	}
	home := t.TempDir()
	dir := filepath.Join(home, "MyApp", ".rift") // baked case-preserved
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mani := `{"secrets":[{"key":"std:aws"}]}`
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(mani), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", readConfigScript("myapp")) // lowercased id
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(string(out), "CONFIG "+base64.StdEncoding.EncodeToString([]byte(mani))) {
		t.Fatalf("manifest not found via case-insensitive dir:\n%s", out)
	}
}

// TestPushScriptBashIntegration exercises the real shell pushScript, which the
// in-process fakeExecer can't cover — in particular the realpath confinement
// guard and tmpfs symlink handling across rotation.
func TestPushScriptBashIntegration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs GNU coreutils (realpath -m) + bash")
	}
	for _, bin := range []string{"bash", "realpath", "mktemp", "sha256sum"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("missing %s", bin)
		}
	}

	storeDir := t.TempDir()
	old := secretsStore
	secretsStore = storeDir
	defer func() { secretsStore = old }()

	// 1. First tmpfs push: lands as a symlink into the store with the content.
	home := t.TempDir()
	if code, e := runPushScript(t, home, ".aws/credentials", "0600", true, "std-aws", "V1"); code != 0 {
		t.Fatalf("first tmpfs push failed (exit %d): %s", code, e)
	}
	dest := filepath.Join(home, ".aws", "credentials")
	if readVia(t, dest) != "V1" {
		t.Fatalf("v1 content wrong: %q", readVia(t, dest))
	}
	if fi, _ := os.Lstat(dest); fi == nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("tmpfs dest is not a symlink")
	}

	// 2. Rotation re-push (the rotation regression): the dest is now a store symlink
	// pointing outside $HOME — the guard must check the parent dir, not the dest,
	// so this must succeed and update the content.
	if code, e := runPushScript(t, home, ".aws/credentials", "0600", true, "std-aws", "V2"); code != 0 {
		t.Fatalf("rotation re-push refused (exit %d): %s — guard followed the final symlink", code, e)
	}
	if readVia(t, dest) != "V2" {
		t.Fatalf("rotation did not update content: %q", readVia(t, dest))
	}

	// 3. Symlinked-parent escape must be refused, and nothing written outside home.
	escape := t.TempDir()
	home2 := t.TempDir()
	if err := os.Symlink(escape, filepath.Join(home2, ".aws")); err != nil {
		t.Fatal(err)
	}
	if code, _ := runPushScript(t, home2, ".aws/credentials", "0600", true, "std-aws", "EVIL"); code == 0 {
		t.Fatal("escape via symlinked parent was allowed")
	}
	if _, err := os.Stat(filepath.Join(escape, "credentials")); err == nil {
		t.Fatal("secret was written outside $HOME via symlinked parent")
	}

	// 4. Persistent push lands a regular file with the mode.
	home3 := t.TempDir()
	if code, e := runPushScript(t, home3, "app/.env", "0640", false, "local-env", "ENV"); code != 0 {
		t.Fatalf("persistent push failed (exit %d): %s", code, e)
	}
	envDest := filepath.Join(home3, "app", ".env")
	if readVia(t, envDest) != "ENV" {
		t.Fatalf("persistent content wrong: %q", readVia(t, envDest))
	}
	if fi, _ := os.Stat(envDest); fi != nil && fi.Mode().Perm() != 0o640 {
		t.Errorf("persistent dest mode = %o, want 640", fi.Mode().Perm())
	}

	// 5. Migrate tmpfs → persistent: the prior store copy must be removed.
	home4 := t.TempDir()
	if code, e := runPushScript(t, home4, ".npmrc", "0600", true, "std-npm", "TOKEN"); code != 0 {
		t.Fatalf("tmpfs push failed (exit %d): %s", code, e)
	}
	if _, err := os.Stat(filepath.Join(storeDir, "std-npm")); err != nil {
		t.Fatalf("store file missing after tmpfs push: %v", err)
	}
	if code, e := runPushScript(t, home4, ".npmrc", "0600", false, "std-npm", "TOKEN"); code != 0 {
		t.Fatalf("persistent migration push failed (exit %d): %s", code, e)
	}
	if _, err := os.Stat(filepath.Join(storeDir, "std-npm")); err == nil {
		t.Error("store copy not removed after migrating off tmpfs")
	}
	if fi, _ := os.Lstat(filepath.Join(home4, ".npmrc")); fi != nil && fi.Mode()&os.ModeSymlink != 0 {
		t.Error("persistent dest is still a symlink after migration")
	}
}
