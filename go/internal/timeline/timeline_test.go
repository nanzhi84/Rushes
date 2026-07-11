package timeline

import (
	"encoding/json"
	"fmt"
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
		{"split", map[string]any{"kind": "split_clip", "timeline_clip_id": "clip_v1_001", "split_frame": 30}, func(value Document) bool {
			return len(value.Tracks[0].Clips) == 3 && value.Tracks[0].Clips[0].TimelineEndFrame == 30 && value.Tracks[0].Clips[1].TimelineStartFrame == 30
		}},
		{"reorder", map[string]any{"kind": "reorder_clip", "timeline_clip_id": "clip_v1_001", "target_frame": 120}, func(value Document) bool {
			return value.Tracks[0].Clips[0].AssetID == "b" && value.Tracks[0].Clips[1].AssetID == "a" && value.DurationFrames == 120
		}},
		{"delete", map[string]any{"kind": "delete_range", "start_frame": 30, "end_frame": 60}, func(value Document) bool { return value.DurationFrames == 90 }},
		{"insert", map[string]any{"kind": "insert_clip", "asset_id": "c", "source_start_frame": 0, "source_end_frame": 30}, func(value Document) bool { return value.DurationFrames == 150 && len(value.Tracks[0].Clips) == 3 }},
		{"replace", map[string]any{"kind": "replace_clip", "timeline_clip_id": "clip_v1_001", "asset_id": "c"}, func(value Document) bool { return value.Tracks[0].Clips[0].AssetID == "c" }},
		{"rate", map[string]any{"kind": "set_playback_rate", "timeline_clip_id": "clip_v1_001", "playback_rate": 2.0}, func(value Document) bool { return value.DurationFrames == 90 }},
		{"gain", map[string]any{"kind": "adjust_gain", "timeline_clip_id": "clip_v1_001", "gain_db": -3.0}, func(value Document) bool { return value.Tracks[0].Clips[0].GainDB == -3 }},
		{"subtitle", map[string]any{"kind": "edit_subtitle_text", "timeline_clip_id": "subtitle", "text": "新字幕"}, func(value Document) bool { return value.Tracks[5].Clips[0].Text == "新字幕" }},
		{"insert subtitle", map[string]any{"kind": "insert_subtitle", "timeline_clip_id": "subtitle_new", "start_frame": 30, "end_frame": 60, "text": "新增字幕"}, func(value Document) bool {
			return len(value.Tracks[5].Clips) == 2 && value.Tracks[5].Clips[1].Text == "新增字幕"
		}},
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

func TestLinkedAudioAndProfessionalEditingOperations(t *testing.T) {
	t.Parallel()
	base, err := ComposeInitial("draft_pro_edit", 1, []Selection{
		{AssetID: "a", AssetKind: "video", SourceEndFrame: 30, Role: "a_roll", HasAudio: true},
		{AssetID: "b", AssetKind: "video", SourceEndFrame: 30, Role: "a_roll", HasAudio: true},
		{AssetID: "c", AssetKind: "video", SourceEndFrame: 30, Role: "a_roll", HasAudio: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Tracks[0].Clips) != 3 || len(base.Tracks[2].Clips) != 3 {
		t.Fatalf("linked tracks=%#v", base.Tracks)
	}
	for index := range base.Tracks[0].Clips {
		visual := base.Tracks[0].Clips[index]
		audio := base.Tracks[2].Clips[index]
		if !visual.Linked || !audio.Linked || visual.ParentBlockID == "" || visual.ParentBlockID != audio.ParentBlockID {
			t.Fatalf("visual=%#v audio=%#v", visual, audio)
		}
	}

	t.Run("split keeps linked audio aligned", func(t *testing.T) {
		result, patchErr := ApplyPatch(base, map[string]any{
			"kind": "split_clip", "timeline_clip_id": "clip_v1_001", "split_frame": 15,
		})
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		if len(result.Tracks[0].Clips) != 4 || len(result.Tracks[2].Clips) != 4 {
			t.Fatalf("visual=%#v audio=%#v", result.Tracks[0].Clips, result.Tracks[2].Clips)
		}
		visualRight := result.Tracks[0].Clips[1]
		audioRight := result.Tracks[2].Clips[1]
		if visualRight.TimelineStartFrame != 15 || audioRight.TimelineStartFrame != 15 ||
			visualRight.ParentBlockID != audioRight.ParentBlockID || visualRight.ParentBlockID == base.Tracks[0].Clips[0].ParentBlockID {
			t.Fatalf("visualRight=%#v audioRight=%#v", visualRight, audioRight)
		}
	})

	t.Run("reorder keeps linked audio aligned", func(t *testing.T) {
		result, patchErr := ApplyPatch(base, map[string]any{
			"kind": "move_clip", "timeline_clip_id": "clip_v1_001", "target_frame": 90,
			"target_track_id": "visual_base", "mode": "insert",
		})
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		for index, expected := range []string{"b", "c", "a"} {
			visual := result.Tracks[0].Clips[index]
			audio := result.Tracks[2].Clips[index]
			if visual.AssetID != expected || audio.AssetID != expected ||
				visual.TimelineStartFrame != audio.TimelineStartFrame || visual.TimelineEndFrame != audio.TimelineEndFrame {
				t.Fatalf("index=%d visual=%#v audio=%#v", index, visual, audio)
			}
		}
	})

	t.Run("track controls persist and locking blocks edits", func(t *testing.T) {
		controlled, patchErr := ApplyPatch(base, map[string]any{
			"kind": "set_track_state", "track_id": "original_audio",
			"muted": true, "solo": true, "gain_db": -6.0,
		})
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		audioTrack := controlled.Tracks[2]
		if !audioTrack.Muted || !audioTrack.Solo || audioTrack.GainDB != -6 {
			t.Fatalf("audio track=%#v", audioTrack)
		}
		locked, patchErr := ApplyPatch(base, map[string]any{
			"kind": "set_track_state", "track_id": "original_audio", "locked": true,
		})
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		if _, patchErr = ApplyPatch(locked, map[string]any{
			"kind": "adjust_gain", "timeline_clip_id": "clip_v1_001_audio", "gain_db": -3.0,
		}); patchErr == nil || !strings.Contains(patchErr.Error(), "已锁定") {
			t.Fatalf("locked edit err=%v", patchErr)
		}
		if _, patchErr = ApplyPatch(base, map[string]any{
			"kind": "set_track_state", "track_id": "visual_base", "muted": true,
		}); patchErr == nil {
			t.Fatal("主视觉轨静音应失败")
		}
	})

	t.Run("unlinked audio moves across audio tracks", func(t *testing.T) {
		unlinked, patchErr := ApplyPatch(base, map[string]any{
			"kind": "set_clip_linked", "timeline_clip_id": "clip_v1_001_audio", "linked": false,
		})
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		result, patchErr := ApplyPatch(unlinked, map[string]any{
			"kind": "move_clip", "timeline_clip_id": "clip_v1_001_audio",
			"target_track_id": "voiceover", "target_frame": 15, "mode": "overwrite",
		})
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		if len(result.Tracks[2].Clips) != 2 || len(result.Tracks[3].Clips) != 1 {
			t.Fatalf("original=%#v voiceover=%#v", result.Tracks[2].Clips, result.Tracks[3].Clips)
		}
		moved := result.Tracks[3].Clips[0]
		if moved.TrackID != "voiceover" || moved.TimelineStartFrame != 15 || moved.TimelineEndFrame != 45 || moved.Linked {
			t.Fatalf("moved=%#v", moved)
		}
		if report := Validate(result); !report.Valid {
			t.Fatalf("report=%#v", report)
		}
	})

	t.Run("overlay inserts into primary and free clips trim and delete", func(t *testing.T) {
		withOverlay := base
		withOverlay.Tracks = append([]Track(nil), base.Tracks...)
		withOverlay.Tracks[1].Clips = []Clip{{
			TimelineClipID: "overlay_1", TrackID: "visual_overlay", AssetID: "still",
			AssetKind: "image", TimelineStartFrame: 0, TimelineEndFrame: 15,
			SourceStartFrame: 0, SourceEndFrame: 15, PlaybackRate: 1,
		}}
		inserted, patchErr := ApplyPatch(withOverlay, map[string]any{
			"kind": "move_clip", "timeline_clip_id": "overlay_1", "target_track_id": "visual_base",
			"target_frame": 30, "mode": "insert",
		})
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		if inserted.DurationFrames != 105 || len(inserted.Tracks[0].Clips) != 4 || len(inserted.Tracks[1].Clips) != 0 {
			t.Fatalf("inserted=%#v", inserted)
		}
		if report := Validate(inserted); !report.Valid {
			t.Fatalf("report=%#v", report)
		}

		free := base
		free.Tracks = append([]Track(nil), base.Tracks...)
		free.Tracks[3].Clips = []Clip{{
			TimelineClipID: "voice_1", TrackID: "voiceover", AssetID: "voice", AssetKind: "audio",
			TimelineStartFrame: 10, TimelineEndFrame: 40, SourceStartFrame: 0, SourceEndFrame: 30, PlaybackRate: 1,
		}}
		trimmed, patchErr := ApplyPatch(free, map[string]any{
			"kind": "trim_clip_edge", "timeline_clip_id": "voice_1", "edge": "start", "timeline_frame": 15,
		})
		if patchErr != nil || trimmed.Tracks[3].Clips[0].SourceStartFrame != 5 {
			t.Fatalf("trimmed=%#v err=%v", trimmed.Tracks[3].Clips, patchErr)
		}
		deleted, patchErr := ApplyPatch(trimmed, map[string]any{
			"kind": "delete_clip", "timeline_clip_id": "voice_1",
		})
		if patchErr != nil || len(deleted.Tracks[3].Clips) != 0 {
			t.Fatalf("deleted=%#v err=%v", deleted.Tracks[3].Clips, patchErr)
		}
	})
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
		{"kind": "split_clip", "timeline_clip_id": "clip_v1_001", "split_frame": 0},
		{"kind": "split_clip", "timeline_clip_id": "missing", "split_frame": 30},
		{"kind": "reorder_clip", "timeline_clip_id": "clip_v1_001", "target_frame": -1},
		{"kind": "reorder_clip", "timeline_clip_id": "missing", "target_frame": 30},
		{"kind": "replace_clip", "timeline_clip_id": "clip_v1_001"},
		{"kind": "set_playback_rate", "timeline_clip_id": "clip_v1_001", "playback_rate": 9},
		{"kind": "adjust_gain"},
		{"kind": "edit_subtitle_text", "timeline_clip_id": "missing", "text": "x"},
		{"kind": "insert_subtitle", "start_frame": 0, "end_frame": 99, "text": ""},
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

func TestVersionNavigationFollowsParentAndLatestChild(t *testing.T) {
	t.Parallel()
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	document, _ := ComposeInitial("draft_nav", 1, []Selection{{AssetID: "a", SourceEndFrame: 30}})
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO drafts(
			draft_id,name,state_version,status,defaults_json,running_jobs_json,brief_json,
			timeline_current_version,timeline_validated,scratch_memory_json,created_at,updated_at
		) VALUES('draft_nav','d',0,'active','{}','[]','{}',1,1,'{}',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	for version, parent := range []any{nil, 1, 2, 1} {
		document.Version = version + 1
		document.TimelineID = fmt.Sprintf("draft_nav:v%d", version+1)
		data, _ := json.Marshal(document)
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO timeline_versions(timeline_id,draft_id,version,parent_version,document_json,created_at)
			VALUES(?,?,?,?,?,?)`, document.TimelineID, "draft_nav", version+1, parent, string(data), now); err != nil {
			t.Fatal(err)
		}
	}
	navigation, err := Navigation(t.Context(), database, "draft_nav", 1)
	if err != nil || navigation.Parent != nil || navigation.Redo == nil || *navigation.Redo != 4 || navigation.Latest != 4 {
		t.Fatalf("navigation=%#v err=%v", navigation, err)
	}
	navigation, err = Navigation(t.Context(), database, "draft_nav", 3)
	if err != nil || navigation.Parent == nil || *navigation.Parent != 2 || navigation.Redo != nil || navigation.Latest != 4 {
		t.Fatalf("navigation=%#v err=%v", navigation, err)
	}
}
