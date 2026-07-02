package secrets

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// TestReadLineNoReadAhead proves the prompt read consumes exactly up to the
// newline and leaves the rest for the interactive shell (no bufio read-ahead).
func TestReadLineNoReadAhead(t *testing.T) {
	r := strings.NewReader("yes\nls -la\n")
	line, err := readLine(context.Background(), r)
	if err != nil || line != "yes" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	rest, _ := io.ReadAll(r)
	if string(rest) != "ls -la\n" {
		t.Errorf("read-ahead consumed shell input; rest=%q", rest)
	}
}

// TestReadLineCancelable proves a blocked prompt read returns on ctx cancel
// (Ctrl-C) instead of hanging the connect forever.
func TestReadLineCancelable(t *testing.T) {
	pr, pw := io.Pipe() // never written → Read blocks
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := readLine(ctx, pr)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected a ctx error after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readLine did not return after ctx cancel — prompt would hang the shell")
	}
}
