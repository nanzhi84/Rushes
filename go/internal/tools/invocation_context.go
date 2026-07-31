package tools

import "context"

const (
	turnIDKey              contextKey = "rushes_turn_id"
	sourceMessageIDKey     contextKey = "rushes_source_message_id"
	invocationToolNameKey  contextKey = "rushes_invocation_tool_name"
	argumentFingerprintKey contextKey = "rushes_argument_fingerprint"
)

// WithTurnIdentity binds a model tool invocation to the durable user task that
// created it. The values are deliberately unavailable to REST callers so only
// an in-flight Agent turn can create or reuse a model tool receipt.
func WithTurnIdentity(ctx context.Context, turnID, sourceMessageID string) context.Context {
	ctx = context.WithValue(ctx, turnIDKey, turnID)
	return context.WithValue(ctx, sourceMessageIDKey, sourceMessageID)
}

func TurnIdentity(ctx context.Context) (turnID, sourceMessageID string) {
	turnID, _ = ctx.Value(turnIDKey).(string)
	sourceMessageID, _ = ctx.Value(sourceMessageIDKey).(string)
	return turnID, sourceMessageID
}

// WithArgumentFingerprint carries the canonical, typed request hash into the
// reducer transaction that commits a timeline mutation and its receipt.
func WithArgumentFingerprint(ctx context.Context, toolName, fingerprint string) context.Context {
	ctx = context.WithValue(ctx, invocationToolNameKey, toolName)
	return context.WithValue(ctx, argumentFingerprintKey, fingerprint)
}

func InvocationFingerprint(ctx context.Context) (toolName, fingerprint string) {
	toolName, _ = ctx.Value(invocationToolNameKey).(string)
	fingerprint, _ = ctx.Value(argumentFingerprintKey).(string)
	return toolName, fingerprint
}
