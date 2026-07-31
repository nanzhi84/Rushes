package reducer

import "github.com/nanzhi84/Rushes/go/internal/telemetry"

var (
	metricManualTimelineWriteRejectedWhileAgent = telemetry.NewCounter(
		"manual_timeline_write_rejected_while_agent",
	)
	metricCurrentTimelineVersionMismatch = telemetry.NewCounter(
		"current_timeline_version_mismatch",
	)
)

// RecordManualTimelineWriteRejectedWhileAgent is shared by the API fast path
// and the reducer transaction-time race guard. It intentionally has no labels.
func RecordManualTimelineWriteRejectedWhileAgent() {
	metricManualTimelineWriteRejectedWhileAgent.Inc()
}

func RecordCurrentTimelineVersionMismatch() {
	metricCurrentTimelineVersionMismatch.Inc()
}
