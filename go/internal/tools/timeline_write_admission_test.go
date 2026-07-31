package tools

import (
	"errors"
	"testing"
)

func TestTimelineWriteAdmissionNotifiesOnlyCompleteRegisteredCallback(t *testing.T) {
	wantErr := errors.New("lease lost")
	var calls int
	ctx := WithTimelineWriteAdmission(t.Context(), "turn-1", "token-1", func(err error) {
		calls++
		if !errors.Is(err, wantErr) {
			t.Fatalf("callback err=%v", err)
		}
	})
	admission, ok := TimelineWriteAdmissionFromContext(ctx)
	if !ok || admission.TurnID != "turn-1" || admission.LeaseToken != "token-1" {
		t.Fatalf("admission=%#v ok=%t", admission, ok)
	}
	NotifyTimelineWriteLeaseLost(ctx, wantErr)
	if calls != 1 {
		t.Fatalf("callback calls=%d", calls)
	}

	incomplete := WithTimelineWriteAdmission(t.Context(), "turn-1", "", func(error) {
		t.Fatal("incomplete admission callback must not run")
	})
	if _, ok := TimelineWriteAdmissionFromContext(incomplete); ok {
		t.Fatal("incomplete admission was accepted")
	}
	NotifyTimelineWriteLeaseLost(incomplete, wantErr)
	NotifyTimelineWriteLeaseLost(t.Context(), wantErr)
}
