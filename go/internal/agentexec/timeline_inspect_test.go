package agentexec

import (
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestTimelineInspectReportsMissingTimelineWithoutFailure(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_inspect_empty")
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	resultRaw, err := exec.ExecuteTool(
		rushestools.WithDraftID(t.Context(), "draft_inspect_empty"),
		"timeline.inspect",
		rushestools.TimelineInspectInput{},
	)
	if err != nil {
		t.Fatal(err)
	}
	result := resultRaw.(rushestools.ToolResult)
	if result.Status != "succeeded" || result.Data["timeline_exists"] != false ||
		result.Data["duration_frames"] != 0 {
		t.Fatalf("result=%#v", result)
	}
}

func TestTimelineInspectReturnsWaveTwoEditingState(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_inspect_wave_two")
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := agenttest.ComposeTimeline("draft_inspect_wave_two", 1, []agenttest.TimelineSelection{{
		AssetID: "asset_video", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 90,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []map[string]any{
		{"kind": "set_clip_fades", "timeline_clip_id": "clip_v1_001", "fade_in_frames": 6, "fade_out_frames": 12},
		{"kind": "set_track_ducking", "track_id": "bgm", "enabled": true, "duck_db": -9.0, "trigger_tracks": []string{"voiceover", "original_audio"}},
		{"kind": "insert_subtitle", "timeline_clip_id": "subtitle_wave_two", "start_frame": 0, "end_frame": 30, "text": "旧字幕", "style": "top_bar"},
		{"kind": "edit_subtitle_text", "timeline_clip_id": "subtitle_wave_two", "text": "新字幕", "style": "bold_bottom"},
	} {
		document, err = timeline.ApplyPatch(document, operation)
		if err != nil {
			t.Fatalf("operation=%#v err=%v", operation, err)
		}
	}
	if _, err := seedTimelineVersion(exec, t.Context(), "draft_inspect_wave_two", document, "inspect_wave_two_fixture", nil); err != nil {
		t.Fatal(err)
	}
	result, err := exec.toolInspectTimeline(t.Context(), "draft_inspect_wave_two", rushestools.TimelineInspectInput{})
	if err != nil {
		t.Fatal(err)
	}
	tracks := result.Data["tracks"].([]map[string]any)
	byID := map[string]map[string]any{}
	for _, track := range tracks {
		byID[track["track_id"].(string)] = track
	}
	visual := byID["visual_base"]["clips"].([]map[string]any)[0]
	if visual["fade_in_frames"] != 6 || visual["fade_out_frames"] != 12 {
		t.Fatalf("visual=%#v", visual)
	}
	ducking, ok := byID["bgm"]["ducking"].(*timeline.TrackDucking)
	if !ok || !ducking.Enabled || ducking.DuckDB != -9 || len(ducking.TriggerTracks) != 2 {
		t.Fatalf("ducking=%#v", byID["bgm"]["ducking"])
	}
	subtitle := byID["subtitles"]["clips"].([]map[string]any)[0]
	if subtitle["text"] != "新字幕" || subtitle["subtitle_style"] != "bold_bottom" {
		t.Fatalf("subtitle=%#v", subtitle)
	}
}

func TestTimelineInspectAndCheckReadExactOwnedVersionWithoutWrites(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_exact_timeline_read"
	agenttest.CreateAgentDraft(t, database, draftID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		version, endFrame int
	}{{1, 30}, {2, 75}} {
		document, composeErr := agenttest.ComposeTimeline(draftID, fixture.version, []agenttest.TimelineSelection{{
			AssetID: "asset_exact", AssetKind: "video",
			SourceStartFrame: 0, SourceEndFrame: fixture.endFrame,
		}})
		if composeErr != nil {
			t.Fatal(composeErr)
		}
		if _, persistErr := seedTimelineVersion(exec,
			t.Context(), draftID, document, "exact_read_fixture", nil,
		); persistErr != nil {
			t.Fatal(persistErr)
		}
	}
	var beforeEvents, beforeVersions, beforeStateVersion int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT
			(SELECT COUNT(*) FROM event_log),
			(SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?),
			(SELECT state_version FROM drafts WHERE draft_id=?)`,
		draftID, draftID,
	).Scan(&beforeEvents, &beforeVersions, &beforeStateVersion); err != nil {
		t.Fatal(err)
	}

	inspect, err := exec.toolInspectTimeline(
		t.Context(), draftID, rushestools.TimelineInspectInput{TimelineID: draftID + ":v1"},
	)
	if err != nil || inspect.Status != string(rushestools.StatusSucceeded) ||
		inspect.Data["timeline_id"] != draftID+":v1" ||
		inspect.Data["timeline_version"] != 1 ||
		inspect.Data["duration_frames"] != 30 {
		t.Fatalf("inspect=%#v err=%v", inspect, err)
	}
	check, err := exec.toolCheckTimeline(
		t.Context(), draftID, rushestools.TimelineCheckInput{TimelineID: draftID + ":v1"},
	)
	if err != nil || check.Status != string(rushestools.StatusSucceeded) ||
		check.Data["timeline_id"] != draftID+":v1" ||
		check.Data["timeline_version"] != 1 {
		t.Fatalf("check=%#v err=%v", check, err)
	}
	for _, result := range []rushestools.ToolResult{
		func() rushestools.ToolResult {
			value, callErr := exec.toolInspectTimeline(
				t.Context(), draftID,
				rushestools.TimelineInspectInput{TimelineID: "other_draft:v1"},
			)
			if callErr != nil {
				t.Fatal(callErr)
			}
			return value
		}(),
		func() rushestools.ToolResult {
			value, callErr := exec.toolCheckTimeline(
				t.Context(), draftID,
				rushestools.TimelineCheckInput{TimelineID: draftID + ":v999"},
			)
			if callErr != nil {
				t.Fatal(callErr)
			}
			return value
		}(),
	} {
		if result.Status != string(rushestools.StatusFailed) ||
			result.Data["error_code"] != string(rushestools.ErrCodeStaleTarget) {
			t.Fatalf("unowned/not-found result=%#v", result)
		}
	}

	var afterEvents, afterVersions, afterStateVersion int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT
			(SELECT COUNT(*) FROM event_log),
			(SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?),
			(SELECT state_version FROM drafts WHERE draft_id=?)`,
		draftID, draftID,
	).Scan(&afterEvents, &afterVersions, &afterStateVersion); err != nil {
		t.Fatal(err)
	}
	if beforeEvents != afterEvents || beforeVersions != afterVersions ||
		beforeStateVersion != afterStateVersion {
		t.Fatalf(
			"read tools wrote state: before=(%d,%d,%d) after=(%d,%d,%d)",
			beforeEvents, beforeVersions, beforeStateVersion,
			afterEvents, afterVersions, afterStateVersion,
		)
	}
}
