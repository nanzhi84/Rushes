package tools

import "context"

type timelineWriteAdmissionContextKey struct{}

// TimelineWriteAdmission is harness-owned and never model-visible. It binds an
// Agent timeline mutation to the exact persisted turn lease; OnLost lets the
// reducer fence cancel the entire turn instead of returning a recoverable tool
// observation that the model could retry.
type TimelineWriteAdmission struct {
	TurnID     string
	LeaseToken string
	OnLost     func(error)
}

func WithTimelineWriteAdmission(
	ctx context.Context,
	turnID, leaseToken string,
	onLost func(error),
) context.Context {
	return context.WithValue(ctx, timelineWriteAdmissionContextKey{}, TimelineWriteAdmission{
		TurnID: turnID, LeaseToken: leaseToken, OnLost: onLost,
	})
}

func TimelineWriteAdmissionFromContext(ctx context.Context) (TimelineWriteAdmission, bool) {
	value, ok := ctx.Value(timelineWriteAdmissionContextKey{}).(TimelineWriteAdmission)
	return value, ok && value.TurnID != "" && value.LeaseToken != ""
}

func NotifyTimelineWriteLeaseLost(ctx context.Context, err error) {
	if admission, ok := TimelineWriteAdmissionFromContext(ctx); ok && admission.OnLost != nil {
		admission.OnLost(err)
	}
}
