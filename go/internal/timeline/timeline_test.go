package timeline

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func TestComposeValidateInspectAndStore(t *testing.T) {
	t.Parallel()
	document, err := ComposeInitial("draft_timeline", 1, []Selection{
		{AssetID: "a", SourceStartFrame: 0, SourceEndFrame: 60, Role: "a_roll"},
		{AssetID: "b", SourceStartFrame: 30, SourceEndFrame: 75, Role: "b_roll"},
	})
	if err != nil {
		t.Fatal(err)
	}
	report := Validate(document)
	if !report.Valid || document.DurationFrames != 105 || len(document.Tracks) != 6 {
		t.Fatalf("document=%#v report=%#v", document, report)
	}
	if summary := Inspect(document); !strings.Contains(summary, "3.50 秒") || !strings.Contains(summary, "主视觉 2 段") {
		t.Fatalf("summary=%s", summary)
	}
	document.Tracks[0].Clips[1].TimelineStartFrame++
	if invalid := Validate(document); invalid.Valid || len(invalid.Issues) == 0 {
		t.Fatalf("invalid=%#v", invalid)
	}

	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	document.Tracks[0].Clips[1].TimelineStartFrame--
	data, _ := json.Marshal(document)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO drafts(
			draft_id,name,state_version,status,defaults_json,running_jobs_json,brief_json,
			timeline_current_version,timeline_validated,scratch_memory_json,created_at,updated_at
		) VALUES('draft_timeline','d',0,'active','{}','[]','{}',1,1,'{}',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO timeline_versions(timeline_id,draft_id,version,document_json,created_at)
		VALUES('draft_timeline:v1','draft_timeline',1,?,?)`, string(data), now); err != nil {
		t.Fatal(err)
	}
	loaded, err := Latest(t.Context(), database, "draft_timeline")
	if err != nil || loaded.DurationFrames != 105 {
		t.Fatalf("loaded=%#v err=%v", loaded, err)
	}
	if next, err := NextVersion(t.Context(), database, "draft_timeline"); err != nil || next != 2 {
		t.Fatalf("next=%d err=%v", next, err)
	}
}

func TestApplyPatchSubsetTable(t *testing.T) {
	t.Parallel()
	base, err := ComposeInitial("draft_patch", 1, []Selection{
		{AssetID: "a", SourceStartFrame: 0, SourceEndFrame: 60, Role: "a_roll"},
		{AssetID: "b", SourceStartFrame: 0, SourceEndFrame: 60, Role: "b_roll"},
	})
	if err != nil {
		t.Fatal(err)
	}
	base.Tracks[1].Clips = []Clip{{
		TimelineClipID: "overlay", TrackID: "visual_overlay", AssetID: "a",
		TimelineStartFrame: 0, TimelineEndFrame: 30, SourceStartFrame: 0, SourceEndFrame: 30, PlaybackRate: 1,
	}}
	base.Tracks[5].Clips = []Clip{{
		TimelineClipID: "subtitle", TrackID: "subtitles", Text: "旧字幕",
		TimelineStartFrame: 0, TimelineEndFrame: 30,
	}}
	tests := []struct {
		name  string
		op    map[string]any
		check func(Document) bool
	}{
		{"trim", map[string]any{"kind": "trim_clip", "timeline_clip_id": "clip_v1_001", "source_start_frame": 15, "source_end_frame": 45}, func(value Document) bool { return value.DurationFrames == 90 }},
		{"delete", map[string]any{"kind": "delete_range", "start_frame": 30, "end_frame": 60}, func(value Document) bool { return value.DurationFrames == 90 }},
		{"insert", map[string]any{"kind": "insert_clip", "asset_id": "c", "source_start_frame": 0, "source_end_frame": 30}, func(value Document) bool { return value.DurationFrames == 150 && len(value.Tracks[0].Clips) == 3 }},
		{"replace", map[string]any{"kind": "replace_clip", "timeline_clip_id": "clip_v1_001", "asset_id": "c"}, func(value Document) bool { return value.Tracks[0].Clips[0].AssetID == "c" }},
		{"rate", map[string]any{"kind": "set_playback_rate", "timeline_clip_id": "clip_v1_001", "playback_rate": 2.0}, func(value Document) bool { return value.DurationFrames == 90 }},
		{"gain", map[string]any{"kind": "adjust_gain", "timeline_clip_id": "clip_v1_001", "gain_db": -3.0}, func(value Document) bool { return value.Tracks[0].Clips[0].GainDB == -3 }},
		{"subtitle", map[string]any{"kind": "edit_subtitle_text", "timeline_clip_id": "subtitle", "text": "新字幕"}, func(value Document) bool { return value.Tracks[5].Clips[0].Text == "新字幕" }},
		{"clear overlay", map[string]any{"kind": "remove_track_clips", "track_id": "visual_overlay"}, func(value Document) bool { return len(value.Tracks[1].Clips) == 0 }},
	}
	for _, item := range tests {
		item := item
		t.Run(item.name, func(t *testing.T) {
			result, err := ApplyPatch(base, item.op)
			if err != nil || result.Version != 2 || !item.check(result) {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
	if _, err := ApplyPatch(base, map[string]any{"kind": "unknown"}); err == nil {
		t.Fatal("未知 patch op 应失败")
	}
}

func TestValidationAndPatchFailureBranches(t *testing.T) {
	t.Parallel()
	document := Empty("draft_invalid", 0)
	document.FPS = 0
	document.DurationFrames = 0
	document.Tracks = append(document.Tracks, document.Tracks[0])
	document.Tracks[0].Clips = []Clip{{
		TimelineClipID: "bad", TrackID: "visual_base", AssetID: "asset",
		TimelineStartFrame: -1, TimelineEndFrame: 2, SourceStartFrame: 3, SourceEndFrame: 2, PlaybackRate: -1,
	}}
	document.Tracks = document.Tracks[:2]
	report := Validate(document)
	if report.Valid || len(report.Issues) < 6 {
		t.Fatalf("report=%#v", report)
	}

	base, err := ComposeInitial("draft_errors", 1, []Selection{{AssetID: "a", SourceEndFrame: 60}})
	if err != nil {
		t.Fatal(err)
	}
	badOperations := []map[string]any{
		{"kind": "trim_clip", "timeline_clip_id": "clip_v1_001", "source_start_frame": 60, "source_end_frame": 30},
		{"kind": "delete_range", "start_frame": -1, "end_frame": 30},
		{"kind": "insert_clip", "asset_id": "", "source_start_frame": 0, "source_end_frame": 30},
		{"kind": "insert_clip", "asset_id": "a", "source_start_frame": 0, "source_end_frame": 30, "track_id": "missing"},
		{"kind": "trim_clip", "timeline_clip_id": "clip_v1_001", "source_start_frame": 0.5, "source_end_frame": 30},
		{"kind": "trim_clip", "timeline_clip_id": "clip_v1_001", "source_start_frame": "0", "source_end_frame": 30},
		{"kind": "trim_clip", "timeline_clip_id": "clip_v1_001", "source_start_s": 0, "source_end_s": 1},
		{"kind": "replace_clip", "timeline_clip_id": "clip_v1_001"},
		{"kind": "set_playback_rate", "timeline_clip_id": "clip_v1_001", "playback_rate": 9},
		{"kind": "adjust_gain"},
		{"kind": "edit_subtitle_text", "timeline_clip_id": "missing", "text": "x"},
		{"kind": "remove_track_clips", "track_id": "visual_base"},
	}
	for _, operation := range badOperations {
		if _, err := ApplyPatch(base, operation); err == nil {
			t.Fatalf("operation should fail: %#v", operation)
		}
	}
	for index, selection := range []Selection{
		{}, {AssetID: "a", SourceStartFrame: -1, SourceEndFrame: 1}, {AssetID: "a", SourceStartFrame: 2, SourceEndFrame: 1},
	} {
		if _, err := ComposeInitial("draft", 1, []Selection{selection}); err == nil {
			t.Fatalf("selection[%d] should fail", index)
		}
	}
	if _, err := ComposeInitial("", 1, []Selection{{AssetID: "a", SourceEndFrame: 1}}); err == nil {
		t.Fatal("empty draft should fail")
	}
}

func TestDeleteRangeHandlesEveryOverlapShape(t *testing.T) {
	t.Parallel()
	document := Empty("draft_overlap", 1)
	document.DurationFrames = 120
	document.Tracks[0].Clips = []Clip{{
		TimelineClipID: "cover", TrackID: "visual_base", AssetID: "a",
		TimelineStartFrame: 0, TimelineEndFrame: 120, SourceStartFrame: 0, SourceEndFrame: 120, PlaybackRate: 1,
	}}
	document.Tracks[1].Clips = []Clip{
		{TimelineClipID: "before", TimelineStartFrame: 0, TimelineEndFrame: 20},
		{TimelineClipID: "left", TimelineStartFrame: 10, TimelineEndFrame: 40, SourceStartFrame: 0, SourceEndFrame: 30},
		{TimelineClipID: "inside", TimelineStartFrame: 35, TimelineEndFrame: 45},
		{TimelineClipID: "right", TimelineStartFrame: 50, TimelineEndFrame: 100, SourceStartFrame: 0, SourceEndFrame: 50},
		{TimelineClipID: "after", TimelineStartFrame: 100, TimelineEndFrame: 120},
	}
	result, err := ApplyPatch(document, map[string]any{"kind": "delete_range", "start_frame": 30, "end_frame": 60})
	if err != nil {
		t.Fatal(err)
	}
	if result.DurationFrames != 90 || len(result.Tracks[1].Clips) != 4 {
		t.Fatalf("result=%#v", result.Tracks[1].Clips)
	}
}

func TestTimelineStoreMissingAndPreviewLookup(t *testing.T) {
	t.Parallel()
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := Get(t.Context(), database, "missing", 1); err != storage.ErrNotFound {
		t.Fatalf("get missing err=%v", err)
	}
	if _, err := Latest(t.Context(), database, "missing"); err != storage.ErrNotFound {
		t.Fatalf("latest missing err=%v", err)
	}
	if preview, err := LatestPreviewID(t.Context(), database, "missing", 1); err != nil || preview != nil {
		t.Fatalf("missing preview=%v err=%v", preview, err)
	}

	document, _ := ComposeInitial("draft_preview", 1, []Selection{{AssetID: "a", SourceEndFrame: 30}})
	data, _ := json.Marshal(document)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	hash := strings.Repeat("a", 64)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO drafts(draft_id,name,state_version,status,defaults_json,running_jobs_json,brief_json,
			timeline_current_version,timeline_validated,scratch_memory_json,created_at,updated_at)
		VALUES('draft_preview','d',0,'active','{}','[]','{}',1,1,'{}',?,?);
		INSERT INTO timeline_versions(timeline_id,draft_id,version,document_json,created_at)
		VALUES('draft_preview:v1','draft_preview',1,?,?);
		INSERT INTO objects(hash,rel_path,size,created_at) VALUES(?, ?, 1, ?);
		INSERT INTO previews(preview_id,draft_id,timeline_version,object_hash,quality_json,created_at)
		VALUES('preview_1','draft_preview',1,?,'{}',?)`,
		now, now, string(data), now, hash, hash, now, hash, now,
	); err != nil {
		t.Fatal(err)
	}
	preview, err := LatestPreviewID(t.Context(), database, "draft_preview", 1)
	if err != nil || preview == nil || *preview != "preview_1" {
		t.Fatalf("preview=%v err=%v", preview, err)
	}
	if mapped, err := ToMap(document); err != nil || mapped["draft_id"] != "draft_preview" {
		t.Fatalf("map=%#v err=%v", mapped, err)
	}
	for _, value := range []any{float64(1), float32(2), 3, int64(4), "bad"} {
		_ = numberValue(value)
	}
	if valueOr("  ", "fallback") != "fallback" || valueOr("value", "fallback") != "value" {
		t.Fatal("valueOr mismatch")
	}
}
