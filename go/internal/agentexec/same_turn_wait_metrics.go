package agentexec

import (
	"log/slog"
	"time"
)

func (exec *Executor) observeSameTurnToolWait(kind, status string, started time.Time) {
	duration := time.Since(started)
	if exec != nil && exec.sameTurnWait != nil {
		exec.sameTurnWait(kind, status, duration)
	}
	slog.Info(
		"same_turn_tool_wait",
		"kind", kind, "status", status, "duration_ms", duration.Milliseconds(),
	)
}
