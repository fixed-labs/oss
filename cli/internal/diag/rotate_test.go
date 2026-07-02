package diag

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRotatingFileLazyCreate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "rift.log")
	rf := newRotatingFile(p, 1<<20, 3)
	// No write yet → no file (and no parent dir) created.
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("logfile created before first write: stat err=%v", err)
	}
	if _, err := rf.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	defer rf.Close()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read after write: %v", err)
	}
	if string(b) != "hello\n" {
		t.Fatalf("content = %q, want %q", b, "hello\n")
	}
}

func TestRotatingFileAppendsAcrossOpens(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rift.log")
	rf1 := newRotatingFile(p, 1<<20, 3)
	if _, err := rf1.Write([]byte("one\n")); err != nil {
		t.Fatal(err)
	}
	rf1.Close()
	// A fresh handle to the same path must append, not truncate (O_APPEND + the
	// size read in openLocked).
	rf2 := newRotatingFile(p, 1<<20, 3)
	if _, err := rf2.Write([]byte("two\n")); err != nil {
		t.Fatal(err)
	}
	rf2.Close()
	b, _ := os.ReadFile(p)
	if string(b) != "one\ntwo\n" {
		t.Fatalf("content = %q, want %q", b, "one\ntwo\n")
	}
}

func TestRotatingFileRotatesAndRetains(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "rift.log")
	// maxBytes 5 == one "xxxx\n" line: writing a second line would exceed the cap,
	// so every write after the first rotates. maxBackups 2 caps retained backups.
	rf := newRotatingFile(p, 5, 2)
	defer rf.Close()
	lines := []string{"aaaa\n", "bbbb\n", "cccc\n", "dddd\n"}
	for _, l := range lines {
		if _, err := rf.Write([]byte(l)); err != nil {
			t.Fatalf("write %q: %v", l, err)
		}
	}
	// Live file holds the last write.
	if b, _ := os.ReadFile(p); string(b) != "dddd\n" {
		t.Fatalf("live = %q, want %q", b, "dddd\n")
	}
	// .1 and .2 retained; .3 must NOT exist (capped at maxBackups=2).
	if b, _ := os.ReadFile(p + ".1"); string(b) != "cccc\n" {
		t.Fatalf(".1 = %q, want %q", b, "cccc\n")
	}
	if b, _ := os.ReadFile(p + ".2"); string(b) != "bbbb\n" {
		t.Fatalf(".2 = %q, want %q", b, "bbbb\n")
	}
	if _, err := os.Stat(p + ".3"); !os.IsNotExist(err) {
		t.Fatalf(".3 should have been dropped (maxBackups=2), stat err=%v", err)
	}
}

func TestRotatingFileConcurrentWrites(t *testing.T) {
	// Run with -race: the mutex must serialize concurrent writers.
	p := filepath.Join(t.TempDir(), "rift.log")
	rf := newRotatingFile(p, 4<<10, 3)
	defer rf.Close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := rf.Write([]byte(strings.Repeat("x", 16) + "\n")); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
