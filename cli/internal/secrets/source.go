package secrets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const maxSecretBytes = 1 << 20 // 1 MiB — secrets are small; bound the push.

// ReadSource resolves a resolved secret's Source to its bytes, on the customer
// side (the laptop, today). It is the broker handler's entry point for
// reading an inject secret's source before ExtractCredential maps the bytes onto
// named env values; the in-package push path uses the unexported readSource. The
// command form runs only user-config commands (never the repo's), so it is safe.
func ReadSource(ctx context.Context, s Source) ([]byte, error) { return readSource(ctx, s) }

// readSource resolves a source to its bytes, on the LAPTOP: a file read, or the
// stdout of a command. The command comes only from the user's config (never the
// repo), so running it is safe. Forward sources have no bytes (handled upstream).
func readSource(ctx context.Context, s Source) ([]byte, error) {
	switch {
	case s.Cmd != "":
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, "sh", "-c", s.Cmd)
		var out, errb bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("source command failed: %v: %s", err, strings.TrimSpace(errb.String()))
		}
		if out.Len() > maxSecretBytes {
			return nil, fmt.Errorf("source command output exceeds %d bytes", maxSecretBytes)
		}
		return out.Bytes(), nil
	case s.Path != "":
		p, err := expandHome(s.Path)
		if err != nil {
			return nil, err
		}
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if fi.IsDir() {
			return nil, fmt.Errorf("source %s is a directory", p)
		}
		if fi.Size() > maxSecretBytes {
			return nil, fmt.Errorf("source file %s exceeds %d bytes", p, maxSecretBytes)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		return b, nil
	default:
		return nil, fmt.Errorf("empty source")
	}
}

func expandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
