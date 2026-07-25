package agenttest

import (
	"github.com/nanzhi84/Rushes/go/internal/agenttest/timelinefixture"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

type TimelineSelection = timelinefixture.Selection

// ComposeTimeline builds a deterministic multi-clip document for tests that
// need a pre-existing timeline.
func ComposeTimeline(
	draftID string,
	version int,
	selections []TimelineSelection,
) (timeline.Document, error) {
	return timelinefixture.Compose(draftID, version, selections)
}
