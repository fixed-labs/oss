// Package kongx is the shared kong-parsing helper for the fixed-labs CLIs: a
// single Parse entry point that both rift and fplctl use to parse a per-command
// flag struct without kong calling os.Exit. Import kong; stdlib otherwise.
package kongx

import (
	"errors"

	"github.com/alecthomas/kong"
)

// ErrHelp is returned by Parse when the user passed --help/-h: kong has already
// printed the help text, so the command should stop cleanly. Callers treat it
// as a zero-exit (like the old flag.ErrHelp path, but exit 0).
var ErrHelp = errors.New("help shown")

// Parse parses args into dest (a per-command flag struct with kong tags) using
// kong, returning an error instead of calling os.Exit. It replaces the
// per-command flag.FlagSet + parseInterleaved/splitRepoFlag: kong parses flags
// interspersed with positionals natively. On --help it returns ErrHelp (help
// already printed). name is the command name for kong's messages.
func Parse(name string, dest any, args []string) error {
	helped := false
	k, err := kong.New(dest, kong.Name(name), kong.Exit(func(int) { helped = true }))
	if err != nil {
		return err
	}
	if _, err := k.Parse(args); err != nil {
		return err
	}
	if helped {
		return ErrHelp
	}
	return nil
}
