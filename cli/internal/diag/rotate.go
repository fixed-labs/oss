package diag

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// rotatingFile is a minimal size-based rotating log sink: an io.WriteCloser that
// appends to a file and, when a write would push it past maxBytes, renames the
// live file to a numbered backup (.1, .2, …) and starts fresh, retaining at most
// maxBackups older files. It is concurrent-safe and opens the file LAZILY on the
// first write, so a devbox command that logs nothing never creates the file.
//
// This is a deliberately small, stdlib-only stand-in for a rotation library
// (e.g. natefinch/lumberjack): the CLI's diagnostic volume is low and one process
// owns the file at a time, so the heavy machinery isn't warranted — and keeping
// the devbox module dependency-free avoids a vendored dep plus a nix vendorHash
// recompute for ~60 lines of well-understood logic.
type rotatingFile struct {
	path       string
	maxBytes   int64
	maxBackups int

	mu   sync.Mutex
	f    *os.File
	size int64
}

func newRotatingFile(path string, maxBytes int64, maxBackups int) *rotatingFile {
	return &rotatingFile{path: path, maxBytes: maxBytes, maxBackups: maxBackups}
}

// Write appends p, rotating first if it would cross maxBytes. A single write is
// never split across files (so one log record stays intact in one file), which
// means the live file can exceed maxBytes by up to one record — acceptable for a
// soft size cap.
func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		if err := r.openLocked(); err != nil {
			return 0, err
		}
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// probe forces the file open once (creating the dir), so Setup can detect an
// unwritable log location up front and fall back to stderr.
func (r *rotatingFile) probe() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil {
		return nil
	}
	return r.openLocked()
}

func (r *rotatingFile) openLocked() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	// 0600: diagnostics can incidentally name hosts/ids; owner-only is the safe
	// default for anything devbox writes under the user's state dir.
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.f, r.size = f, info.Size()
	return nil
}

// rotateLocked closes the live file, shifts the numbered backups up by one
// (dropping the oldest past maxBackups), renames the live file to .1, and reopens
// a fresh live file. Rename/remove errors are tolerated: rotation must never lose
// the ability to keep logging.
func (r *rotatingFile) rotateLocked() error {
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
	if r.maxBackups < 1 {
		_ = os.Remove(r.path)
		return r.openLocked()
	}
	_ = os.Remove(r.backupPath(r.maxBackups)) // drop the oldest
	for i := r.maxBackups - 1; i >= 1; i-- {
		_ = os.Rename(r.backupPath(i), r.backupPath(i+1))
	}
	_ = os.Rename(r.path, r.backupPath(1))
	return r.openLocked()
}

func (r *rotatingFile) backupPath(i int) string {
	return r.path + "." + strconv.Itoa(i)
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
