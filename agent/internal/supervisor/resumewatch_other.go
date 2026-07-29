//go:build !linux

package supervisor

import "log/slog"

// newTimerfdWatch is Linux-only: timerfd_create + TFD_TIMER_CANCEL_ON_SET have
// no portable equivalent. This stub exists so `go build` / `go test ./...` work
// on a developer machine; the agent itself only ever runs inside a Fly (Linux)
// VM, so the plain timer newResumeWatch selects here is never a production path.
func newTimerfdWatch(*slog.Logger) (resumeWatch, error) { return nil, errUnsupportedPlatform }
