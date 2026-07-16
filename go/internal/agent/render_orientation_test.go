package agent

import (
	"encoding/json"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestRenderOrientationParticipatesInIdempotencyWithoutNumericKnobs(t *testing.T) {
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_orientation")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	document, err := timeline.ComposeInitial("draft_orientation", 1, []timeline.Selection{{
		AssetID: "fixture", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 30, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.persistTimeline(t.Context(), "draft_orientation", document, "orientation_fixture"); err != nil {
		t.Fatal(err)
	}
	ctx := rushestools.WithDraftID(t.Context(), "draft_orientation")
	autoRaw, err := service.ExecuteTool(ctx, "render.preview", rushestools.RenderPreviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	portraitRaw, err := service.ExecuteTool(ctx, "render.preview", rushestools.RenderPreviewInput{Orientation: "portrait"})
	if err != nil {
		t.Fatal(err)
	}
	portraitAgainRaw, err := service.ExecuteTool(ctx, "render.preview", rushestools.RenderPreviewInput{Orientation: "portrait"})
	if err != nil {
		t.Fatal(err)
	}
	landscapeRaw, err := service.ExecuteTool(ctx, "render.preview", rushestools.RenderPreviewInput{Orientation: "landscape"})
	if err != nil {
		t.Fatal(err)
	}
	auto := autoRaw.(rushestools.ToolResult)
	portrait := portraitRaw.(rushestools.ToolResult)
	portraitAgain := portraitAgainRaw.(rushestools.ToolResult)
	landscape := landscapeRaw.(rushestools.ToolResult)
	if auto.Data["job_id"] == portrait.Data["job_id"] || portrait.Data["job_id"] == landscape.Data["job_id"] ||
		portrait.Data["job_id"] != portraitAgain.Data["job_id"] {
		t.Fatalf("auto=%#v portrait=%#v again=%#v landscape=%#v", auto, portrait, portraitAgain, landscape)
	}
	rows, err := database.Read().QueryContext(t.Context(), `
		SELECT idempotency_key,payload_json FROM jobs WHERE draft_id='draft_orientation' ORDER BY idempotency_key`)
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
	if err := rows.Err(); err != nil || count != 3 || !orientations["auto"] || !orientations["portrait"] || !orientations["landscape"] {
		t.Fatalf("count=%d orientations=%#v err=%v", count, orientations, err)
	}
	if _, err := service.ExecuteTool(ctx, "render.preview", rushestools.RenderPreviewInput{Orientation: "square"}); err == nil {
		t.Fatal("unknown orientation should fail")
	}
}
