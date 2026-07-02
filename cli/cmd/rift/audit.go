package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/fixed-labs/oss/cli/internal/client"
	"github.com/fixed-labs/oss/cli/internal/config"
)

// auditor posts the secret-access audit row to the control plane after a
// `devbox run`, using the box's machine token and the existing events endpoint
// (`POST /api/rift/v1/<workspace-id>/events`). It is best-effort and
// NON-FATAL: a failure to audit must never fail the command — post logs a
// warning to the diagnostic log (diag) and returns. The workspace identity is taken server-side from
// the machine token, so it is not in the body.
//
// In-VM the provisioner injects RIFT_WORKSPACE_ID + the machine token +
// RIFT_API_URL (config.FromEnvOrFile reads them), which is exactly the same
// mechanism the in-VM `devbox suspend/keepalive` self-service verbs reuse — so we
// reuse it here too, rather than inventing a new auth path.
type auditor struct {
	c  *client.Client
	id string // the box's own workspace id (machine-token subject)
}

// newAuditor builds an auditor from the resolved config. If the box is not in a
// machine context (no RIFT_WORKSPACE_ID / token / api url), it returns a
// disabled auditor whose post is a no-op — auditing is best-effort, and a missing
// machine identity must not fail `devbox run`.
func newAuditor(cfg *config.Config) *auditor {
	if cfg == nil || cfg.MachineWorkspaceID == "" || cfg.Token == "" || cfg.APIBaseURL == "" {
		return &auditor{} // disabled — post is a no-op
	}
	return &auditor{c: client.New(cfg.APIBaseURL, cfg.Token), id: cfg.MachineWorkspaceID}
}

// post emits one secret-access event per secret (one row per secret keeps the
// audit indexable by secret, with "the secret" being a per-row field). It is
// best-effort: any failure is logged and swallowed. exitCode is nil for a
// --shell session or a never-launched child (encodes as JSON null).
func (a *auditor) post(ctx context.Context, secs []string, command string, exitCode *int, success bool) {
	if a.c == nil {
		return // disabled
	}
	outcome := "failure"
	if success {
		outcome = "success"
	}
	// Use a fresh short-bounded context: the parent ctx may already be cancelled
	// (the child caught SIGINT), but we still want the audit row to go out.
	pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range secs {
		ev := client.SecretAccessEvent{
			Type:     "secret-access",
			Secret:   s,
			Command:  command,
			ExitCode: exitCode,
			Outcome:  outcome,
		}
		if err := a.c.PostSecretAccess(pctx, a.id, ev); err != nil {
			slog.Warn("secret-access audit post failed (non-fatal)", "secret", s, "err", err)
		}
	}
}
