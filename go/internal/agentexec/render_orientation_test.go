package agentexec

import (
	"encoding/json"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestRenderOrientationParticipatesInIdempotencyWithoutNumericKnobs(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_orientation"
	agenttest.CreateAgentDraft(t, database, draftID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "fixture", AssetKind: "video",
		SourceStartFrame: 0, SourceEndFrame: 30, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedTimelineVersion(exec, t.Context(), draftID, document, "orientation_fixture", nil); err != nil {
		t.Fatal(err)
	}
	start := func(orientation string) rushestools.ToolResult {
		t.Helper()
		result, executeErr := exec.enqueuePreviewRender(
			t.Context(), draftID, orientation, document.TimelineID,
		)
		if executeErr != nil {
			t.Fatal(executeErr)
		}
		return result
	}

	auto := start("")
	portrait := start("portrait")
	portraitAgain := start("portrait")
	landscape := start("landscape")
	if auto.Data["job_id"] == portrait.Data["job_id"] ||
		portrait.Data["job_id"] == landscape.Data["job_id"] ||
		portrait.Data["job_id"] != portraitAgain.Data["job_id"] {
		t.Fatalf("auto=%#v portrait=%#v again=%#v landscape=%#v",
			auto, portrait, portraitAgain, landscape)
	}

	rows, err := database.Read().QueryContext(t.Context(), `
		SELECT idempotency_key,payload_json
		FROM jobs
		WHERE draft_id=?
		ORDER BY idempotency_key`, draftID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	orientations := map[string]bool{}
	count := 0
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			t.Fatal(err)
		}
		payload := map[string]any{}
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatal(err)
		}
		orientation, _ := payload["orientation"].(string)
		orientations[orientation] = true
		if key == "" || payload["timeline_version"] == nil {
			t.Fatalf("key=%q payload=%#v", key, payload)
		}
		count++
	}
	if err := rows.Err(); err != nil ||
		count != 3 ||
		!orientations["auto"] ||
		!orientations["portrait"] ||
		!orientations["landscape"] {
		t.Fatalf("count=%d orientations=%#v err=%v", count, orientations, err)
	}
	var validationEvents int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*)
		FROM event_log
		WHERE draft_id=? AND event_type='TimelineValidated'`, draftID,
	).Scan(&validationEvents); err != nil || validationEvents != 4 {
		t.Fatalf("每个新 render job 必须同批附带 strict validation: events=%d err=%v",
			validationEvents, err)
	}
	if _, err := exec.enqueuePreviewRender(
		t.Context(), draftID, "square", document.TimelineID,
	); err == nil {
		t.Fatal("unknown orientation should fail")
	}
}
