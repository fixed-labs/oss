package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"runtime"
	"testing"
)

// These tests swap package-level vars (writeClipboard) with save + t.Cleanup
// restore, so they must NOT run in parallel.

func TestWriteClipboard(t *testing.T) {
	const url = "https://fixedlabs.dev/activate?code=WXYZP-QRSTU"
	b64 := base64.StdEncoding.EncodeToString([]byte(url))

	t.Run("plain", func(t *testing.T) {
		t.Setenv("TMUX", "")
		var buf bytes.Buffer
		writeClipboard(&buf, url)
		want := "\x1b]52;c;" + b64 + "\x07"
		if got := buf.String(); got != want {
			t.Fatalf("plain OSC 52 bytes\n got %q\nwant %q", got, want)
		}
	})

	t.Run("tmux-wrapped", func(t *testing.T) {
		t.Setenv("TMUX", "/tmp/tmux-1000/default,1234,0")
		var buf bytes.Buffer
		writeClipboard(&buf, url)
		inner := "\x1b]52;c;" + b64 + "\x07"
		// Build the tmux DCS-wrapped expectation with an independent doubling
		// loop (an independent oracle) — do NOT call writeClipboard to derive it.
		doubled := ""
		for _, r := range inner {
			if r == '\x1b' {
				doubled += "\x1b\x1b"
			} else {
				doubled += string(r)
			}
		}
		want := "\x1bPtmux;" + doubled + "\x1b\\"
		if got := buf.String(); got != want {
			t.Fatalf("tmux-wrapped OSC 52 bytes\n got %q\nwant %q", got, want)
		}
	})
}

func TestKeyLoop(t *testing.T) {
	const url = "https://fixedlabs.dev/activate?code=WXYZP-QRSTU"

	// clipboardRecorder counts writeClipboard invocations and records every URL
	// it was handed (not last-write-wins), so multi-copy cases like {'c','c'}
	// have signal.
	type clipboardRecorder struct {
		count int
		urls  []string
	}

	// swapWriteClipboard replaces the package-level writeClipboard with a stub
	// backed by a clipboardRecorder, restoring the original on cleanup.
	swapWriteClipboard := func(t *testing.T) *clipboardRecorder {
		t.Helper()
		orig := writeClipboard
		rec := &clipboardRecorder{}
		writeClipboard = func(w io.Writer, s string) {
			rec.count++
			rec.urls = append(rec.urls, s)
		}
		t.Cleanup(func() { writeClipboard = orig })
		return rec
	}

	t.Run("c copies the url", func(t *testing.T) {
		rec := swapWriteClipboard(t)
		var out bytes.Buffer
		canceled := false
		ret := keyLoop(bytes.NewReader([]byte{'c'}), &out, url, func() { canceled = true })
		if ret {
			t.Fatalf("keyLoop returned true on EOF after 'c'; want false")
		}
		if rec.count != 1 {
			t.Fatalf("writeClipboard called %d times; want 1", rec.count)
		}
		if len(rec.urls) != 1 || rec.urls[0] != url {
			t.Fatalf("writeClipboard got %v; want [%q]", rec.urls, url)
		}
		if canceled {
			t.Fatalf("cancel fired on 'c'; it should not")
		}
	})

	t.Run("C (uppercase) copies the url", func(t *testing.T) {
		rec := swapWriteClipboard(t)
		var out bytes.Buffer
		canceled := false
		ret := keyLoop(bytes.NewReader([]byte{'C'}), &out, url, func() { canceled = true })
		if ret {
			t.Fatalf("keyLoop returned true on EOF after 'C'; want false")
		}
		if rec.count != 1 {
			t.Fatalf("writeClipboard called %d times; want 1", rec.count)
		}
		if len(rec.urls) != 1 || rec.urls[0] != url {
			t.Fatalf("writeClipboard got %v; want [%q]", rec.urls, url)
		}
		if canceled {
			t.Fatalf("cancel fired on 'C'; it should not")
		}
	})

	t.Run("q cancels and returns true", func(t *testing.T) {
		rec := swapWriteClipboard(t)
		var out bytes.Buffer
		canceled := false
		ret := keyLoop(bytes.NewReader([]byte{'q'}), &out, url, func() { canceled = true })
		if !ret {
			t.Fatalf("keyLoop returned false on 'q'; want true")
		}
		if !canceled {
			t.Fatalf("cancel did not fire on 'q'")
		}
		if rec.count != 0 {
			t.Fatalf("writeClipboard called %d times on 'q'; want 0", rec.count)
		}
	})

	t.Run("Q (uppercase) cancels and returns true", func(t *testing.T) {
		rec := swapWriteClipboard(t)
		var out bytes.Buffer
		canceled := false
		ret := keyLoop(bytes.NewReader([]byte{'Q'}), &out, url, func() { canceled = true })
		if !ret {
			t.Fatalf("keyLoop returned false on 'Q'; want true")
		}
		if !canceled {
			t.Fatalf("cancel did not fire on 'Q'")
		}
		if rec.count != 0 {
			t.Fatalf("writeClipboard called %d times on 'Q'; want 0", rec.count)
		}
	})

	t.Run("ctrl-c cancels and returns true", func(t *testing.T) {
		rec := swapWriteClipboard(t)
		var out bytes.Buffer
		canceled := false
		ret := keyLoop(bytes.NewReader([]byte{0x03}), &out, url, func() { canceled = true })
		if !ret {
			t.Fatalf("keyLoop returned false on Ctrl-C; want true")
		}
		if !canceled {
			t.Fatalf("cancel did not fire on Ctrl-C")
		}
		if rec.count != 0 {
			t.Fatalf("writeClipboard called %d times on Ctrl-C; want 0", rec.count)
		}
	})

	t.Run("unmapped byte is ignored", func(t *testing.T) {
		// An unmapped byte (here ESC 0x1b — an arrow-key/escape-sequence lead)
		// must not copy or cancel; the loop keeps reading until EOF → false.
		rec := swapWriteClipboard(t)
		var out bytes.Buffer
		canceled := false
		ret := keyLoop(bytes.NewReader([]byte{0x1b}), &out, url, func() { canceled = true })
		if ret {
			t.Fatalf("keyLoop returned true on unmapped byte; want false")
		}
		if canceled {
			t.Fatalf("cancel fired on unmapped byte; it should not")
		}
		if rec.count != 0 {
			t.Fatalf("writeClipboard called %d times on unmapped byte; want 0", rec.count)
		}
	})

	t.Run("eof returns false", func(t *testing.T) {
		rec := swapWriteClipboard(t)
		var out bytes.Buffer
		canceled := false
		ret := keyLoop(bytes.NewReader(nil), &out, url, func() { canceled = true })
		if ret {
			t.Fatalf("keyLoop returned true on EOF; want false")
		}
		if canceled {
			t.Fatalf("cancel fired on EOF; it should not")
		}
		if rec.count != 0 {
			t.Fatalf("writeClipboard called %d times on EOF; want 0", rec.count)
		}
	})

	t.Run("loop continues after c then q cancels", func(t *testing.T) {
		// 'c' copies and the loop CONTINUES (does not return), so the later 'q'
		// still cancels. A 'c' that erroneously returned would strand the loop.
		rec := swapWriteClipboard(t)
		var out bytes.Buffer
		canceled := false
		ret := keyLoop(bytes.NewReader([]byte{'c', 'q'}), &out, url, func() { canceled = true })
		if !ret {
			t.Fatalf("keyLoop returned false on {'c','q'}; want true")
		}
		if !canceled {
			t.Fatalf("cancel did not fire on {'c','q'}")
		}
		if rec.count != 1 {
			t.Fatalf("writeClipboard called %d times on {'c','q'}; want 1", rec.count)
		}
		if len(rec.urls) != 1 || rec.urls[0] != url {
			t.Fatalf("writeClipboard got %v; want [%q]", rec.urls, url)
		}
	})

	t.Run("loop continues after c then c copies twice", func(t *testing.T) {
		// Two copies must be recorded: 'c' copies and the loop continues, so the
		// second 'c' copies again; EOF then returns false (no cancel).
		rec := swapWriteClipboard(t)
		var out bytes.Buffer
		canceled := false
		ret := keyLoop(bytes.NewReader([]byte{'c', 'c'}), &out, url, func() { canceled = true })
		if ret {
			t.Fatalf("keyLoop returned true on {'c','c'} + EOF; want false")
		}
		if canceled {
			t.Fatalf("cancel fired on {'c','c'}; it should not")
		}
		if rec.count != 2 {
			t.Fatalf("writeClipboard called %d times on {'c','c'}; want 2", rec.count)
		}
		if len(rec.urls) != 2 || rec.urls[0] != url || rec.urls[1] != url {
			t.Fatalf("writeClipboard got %v; want [%q %q]", rec.urls, url, url)
		}
	})
}

func TestShouldAutoOpen(t *testing.T) {
	// envFrom builds a func(string)string over a fixed map for the pure test.
	envFrom := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	cases := []struct {
		name        string
		env         map[string]string
		interactive bool
		noBrowser   bool
		want        bool
		// linuxOnly: assertion only holds on linux (headless-linux rule).
		linuxOnly bool
		// nonLinuxOnly: assertion only holds off linux (empty-env local happy
		// path — on linux empty-env is the headless-suppress row).
		nonLinuxOnly bool
	}{
		{name: "non-interactive", env: nil, interactive: false, noBrowser: false, want: false},
		{name: "no-browser flag", env: nil, interactive: true, noBrowser: true, want: false},
		{name: "ssh connection", env: map[string]string{"SSH_CONNECTION": "1.2.3.4 5 6.7.8.9 22"}, interactive: true, want: false},
		{name: "ssh tty", env: map[string]string{"SSH_TTY": "/dev/pts/0"}, interactive: true, want: false},
		{name: "ssh client", env: map[string]string{"SSH_CLIENT": "1.2.3.4 5 22"}, interactive: true, want: false},
		// SSH suppression dominates even when a display is present.
		{name: "ssh var + DISPLAY both set", env: map[string]string{"SSH_CONNECTION": "1.2.3.4 5 6.7.8.9 22", "DISPLAY": ":0"}, interactive: true, want: false},
		{name: "headless linux (no display)", env: map[string]string{}, interactive: true, want: false, linuxOnly: true},
		{name: "linux with DISPLAY", env: map[string]string{"DISPLAY": ":0"}, interactive: true, want: true, linuxOnly: true},
		{name: "linux with WAYLAND_DISPLAY", env: map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, interactive: true, want: true, linuxOnly: true},
		// darwin/local happy path: interactive, no SSH, empty env → true off linux.
		{name: "local interactive non-linux empty env", env: map[string]string{}, interactive: true, want: true, nonLinuxOnly: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.linuxOnly && runtime.GOOS != "linux" {
				t.Skipf("linux-only case on %s", runtime.GOOS)
			}
			if tc.nonLinuxOnly && runtime.GOOS == "linux" {
				t.Skip("non-linux-only case on linux")
			}
			got := shouldAutoOpen(envFrom(tc.env), tc.interactive, tc.noBrowser)
			if got != tc.want {
				t.Fatalf("shouldAutoOpen(env=%v, interactive=%v, noBrowser=%v) = %v; want %v",
					tc.env, tc.interactive, tc.noBrowser, got, tc.want)
			}
		})
	}
}

func TestErrLoginCanceled(t *testing.T) {
	if got := errLoginCanceled.Error(); got != "login canceled" {
		t.Fatalf("errLoginCanceled.Error() = %q; want %q", got, "login canceled")
	}
	wrapped := fmt.Errorf("wrap: %w", errLoginCanceled)
	if !errors.Is(wrapped, errLoginCanceled) {
		t.Fatalf("errors.Is(fmt.Errorf(\"wrap: %%w\", errLoginCanceled), errLoginCanceled) = false; want true")
	}
}
