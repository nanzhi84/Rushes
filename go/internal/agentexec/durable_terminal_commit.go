package agentexec

import "context"

type durableTerminalCommitKey struct{}

type DurableTerminalCommitFunc func(func() (bool, error)) (bool, error)

// WithDurableTerminalCommit lets executor-side blocking-decision writes share
// the TurnQueue's cancellation fence without importing agent and creating a
// package cycle.
func WithDurableTerminalCommit(ctx context.Context, commit DurableTerminalCommitFunc) context.Context {
	if commit == nil {
		return ctx
	}
	return context.WithValue(ctx, durableTerminalCommitKey{}, commit)
}

// CommitDurableTerminal runs commit under the queue fence when the caller is a
// real queued turn. Direct executor tests and non-queue uses fall back to the
// commit itself.
func CommitDurableTerminal(ctx context.Context, commit func() (bool, error)) (bool, error) {
	if fenced, ok := ctx.Value(durableTerminalCommitKey{}).(DurableTerminalCommitFunc); ok && fenced != nil {
		return fenced(commit)
	}
	return commit()
}
