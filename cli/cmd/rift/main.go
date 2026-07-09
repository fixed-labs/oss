// devbox — the developer CLI for devboxes. A single static
// binary, distributed to laptops AND baked into the workspace image so the
// same binary runs the in-VM resize/keepalive/suspend (config.FromEnvOrFile
// picks up the machine credentials there; the machine bearer only opens the
// self-service agent routes, so in-VM those verbs act on the VM's own
// workspace and the rest of the surface needs a developer login).
//
// login   device-flow magic-link login (mints the developer bearer)
// new     create + connect (infers repo/commit from the cwd git remote)
// ls      live list
// connect resume-if-stopped, attach a persistent session over SSH-over-WireGuard
//
//	(sessions outlive the connection; a reconnect re-attaches the same one)
//
// suspend/resume/rm/keepalive/resize  lifecycle
//
// The connect/new tunnel bring-up (userspace wireguard-go netstack + the SSH
// bridge) is wired in internal/tunnel; the interactive session rides a Go SSH
// client over that netstack (the devbox-session subsystem, internal/session)
// under the client-side chrome compositor (internal/compositor). The live
// handshake against a booted workspace is exercised only by a live end-to-end
// test against a real box.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fixed-labs/oss/cli/internal/client"
	"github.com/fixed-labs/oss/cli/internal/config"
	"github.com/fixed-labs/oss/cli/internal/diag"
)

// version is overridden at release build via -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	// Route background/diagnostic output to a rotating disk logfile rather than
	// the terminal: under `devbox connect`'s compositor (laptop) or `devbox run`
	// inside a box, a stray stdout/stderr write from a background goroutine would
	// trample whatever full-screen program owns the screen. Command results and
	// interactive prompts still go to stdout/stderr directly. Best-effort; the
	// returned close runs on the normal-return path (os.Exit paths don't buffer,
	// so nothing is lost when they skip it).
	closeLog, _ := diag.Setup()
	defer func() { _ = closeLog() }()
	// SIGHUP (terminal/window closed) must be caught alongside SIGINT/SIGTERM:
	// closing the terminal is the most common way a `devbox connect` ends, and
	// Go's default action for an uncaught SIGHUP is to terminate WITHOUT running
	// defers — which skips connect()'s deferred c.Detach() and strands the
	// server-side attachment record until its 12h lease expires. Each reconnect mints a fresh
	// laptop keypair (a NEW row, never a replacement), so strands accumulate and
	// trip the secret broker's "exactly one laptop attached" rule
	// (broker.ErrMultipleLaptops). Catching SIGHUP turns hangup into a clean
	// ctx-cancel so connect() returns and Detach runs. (SIGKILL / power-loss /
	// network-gone remain uncatchable here — the durable backstop for those is
	// relay-side liveness reaping in the control plane, not this signal set.)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "login":
		err = cmdLogin(ctx, args)
	case "ls", "list":
		err = cmdList(ctx, args)
	case "set-default-context":
		err = cmdSetDefaultContext(ctx, args)
	case "set-default-region":
		err = cmdSetDefaultRegion(ctx, args)
	case "sizes":
		err = cmdSizes(ctx, args)
	case "regions":
		err = cmdRegions(ctx, args)
	case "new":
		err = cmdNew(ctx, args)
	case "connect":
		err = cmdConnect(ctx, args)
	case "suspend":
		err = lifecycle(ctx, args, "suspend")
	case "resume":
		err = lifecycle(ctx, args, "resume")
	case "rm", "destroy":
		err = lifecycle(ctx, args, "rm")
	case "resize":
		err = cmdResize(ctx, args)
	case "keepalive":
		err = cmdKeepalive(ctx, args)
	case "secrets":
		err = cmdSecrets(ctx, args)
	case "image":
		err = cmdImage(ctx, args)
	case "run":
		err = cmdRun(ctx, args)
	case "-h", "--help", "help":
		usage()
		return
	case "version", "--version":
		fmt.Println(version)
		return
	default:
		fmt.Fprintf(os.Stderr, "rift: unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "rift: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, "rift — ephemeral developer workspaces\n\n"+
		"usage:\n"+
		"  rift login\n"+
		"  rift new [--size S] [--region R] [--repo REPO] [--forge F] [--context CTX] [--ref BRANCH | --image SHA]\n"+
		"  rift ls [--context CTX]\n"+
		"  rift set-default-context [CTX]\n"+
		"  rift set-default-region [SLUG | --clear]\n"+
		"  rift sizes\n"+
		"  rift regions\n"+
		"  rift connect [--new] [--session NAME] <id>\n"+
		"  rift suspend|resume|rm <id>\n"+
		"  rift resize <id> --size S\n"+
		"  rift keepalive <id> [--for DURATION]\n"+
		"  rift image ls|pin <sha>|unpin <sha>\n"+
		"  rift secrets status|map <key> <source>\n"+
		"  rift run --secret NAME [--secret NAME...] -- CMD   (in-VM: inject a secret)\n"+
		"  rift run --shell --secret NAME ...                 (subshell with secrets)\n"+
		"  rift run --secret NAME --materialize-to PATH       (write secret to a file)\n"+
		"  rift version\n\n"+
		"The API URL + bearer come from `rift login` (~/.config/rift/config.json).\n"+
		"In-VM the provisioner injects a machine token via RIFT_API_URL/RIFT_TOKEN/\n"+
		"RIFT_WORKSPACE_ID; there only suspend/resize/keepalive are available, acting\n"+
		"on the VM's own workspace, and <id> may be omitted.\n")
}

// authedClient builds a client from saved/env config, requiring a valid login.
func authedClient() (*client.Client, *config.Config, error) {
	cfg, err := config.FromEnvOrFile()
	if err != nil {
		return nil, nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	return client.New(cfg.APIBaseURL, cfg.Token), cfg, nil
}

func ctxTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}
