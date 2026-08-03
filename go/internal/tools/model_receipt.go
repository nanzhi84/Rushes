package tools

// CompletionSemantics is the Registry-owned contract for what may end one
// model-visible tool call. It intentionally excludes job lifecycle states:
// pending/running/queued belong to the harness and UI, never to an LLM receipt.
type CompletionSemantics string

const (
	CompletionTerminalOnly          CompletionSemantics = "terminal_only"
	CompletionTerminalOrWaitingUser CompletionSemantics = "terminal_or_waiting_user"
)

func (semantics CompletionSemantics) Valid() bool {
	return semantics == CompletionTerminalOnly ||
		semantics == CompletionTerminalOrWaitingUser
}

// ModelReceiptPolicy is copied from a Registry Spec into the Agent adapter.
// TypedSuccessAdapter is true only for legacy typed read results whose Go
// output schema has no status field; the adapter may add succeeded after the
// Registry has already decoded the exact typed value. Explicit ToolResult
// envelopes must always carry their own status and fail closed when missing.
type ModelReceiptPolicy struct {
	Completion          CompletionSemantics
	TypedSuccessAdapter bool
}

func (policy ModelReceiptPolicy) Valid() bool { return policy.Completion.Valid() }

func (policy ModelReceiptPolicy) Allows(status ToolStatus) bool {
	switch status {
	case StatusSucceeded, StatusRejected, StatusFailed, StatusValidationFailed, StatusCancelled, StatusTimeout:
		return true
	case StatusWaiting:
		return policy.Completion == CompletionTerminalOrWaitingUser
	default:
		return false
	}
}
