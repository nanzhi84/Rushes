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
	if !report.Valid || document.DurationFrames != 105 || len(document.Tracks) != 7 {
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
			timeline_current_version,timeline_validated,created_at,updated_at
		) VALUES('draft_timeline','d',0,'active','{}','[]','{}',1,1,?,?)`, now, now); err != nil {
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

func TestLinkedAVValidationAtomicEditsAndOriginalAudioSync(t *testing.T) {
	t.Parallel()
	base, err := ComposeInitial("draft_linked_integrity", 1, []Selection{
		{AssetID: "talk", AssetKind: "video", SourceStartFrame: 10, SourceEndFrame: 40, Role: "a_roll", HasAudio: true},
		{AssetID: "talk", AssetKind: "video", SourceStartFrame: 60, SourceEndFrame: 90, Role: "a_roll", HasAudio: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	drifted := base
	drifted.Tracks = append([]Track(nil), base.Tracks...)
	drifted.Tracks[2].Clips = append([]Clip(nil), base.Tracks[2].Clips...)
	drifted.Tracks[2].Clips[0].TimelineStartFrame = 5
	drifted.Tracks[2].Clips[0].TimelineEndFrame = 35
	report := Validate(drifted)
	if report.Valid || !validationHasCode(report, "linked_av_timeline_mismatch") {
		t.Fatalf("drifted report=%#v", report)
	}

	repaired, err := ApplyPatch(drifted, map[string]any{
		"kind": "sync_original_audio", "audio_asset_ids": []string{"talk"},
	})
	if err != nil || !Validate(repaired).Valid || len(repaired.Tracks[2].Clips) != len(repaired.Tracks[0].Clips) {
		t.Fatalf("repaired=%#v report=%#v err=%v", repaired.Tracks[2], Validate(repaired), err)
	}

	trimmed, err := ApplyPatch(base, map[string]any{
		"kind": "trim_clip", "timeline_clip_id": "clip_v1_001",
		"source_start_frame": 15, "source_end_frame": 35,
	})
	if err != nil || !Validate(trimmed).Valid || trimmed.DurationFrames != 50 ||
		trimmed.Tracks[0].Clips[0].TimelineEndFrame != 20 ||
		trimmed.Tracks[2].Clips[0].TimelineEndFrame != 20 {
		t.Fatalf("trimmed=%#v audio=%#v report=%#v err=%v", trimmed.Tracks[0], trimmed.Tracks[2], Validate(trimmed), err)
	}

	rated, err := ApplyPatch(base, map[string]any{
		"kind": "set_playback_rate", "timeline_clip_id": "clip_v1_001", "playback_rate": 2.0,
	})
	if err != nil || !Validate(rated).Valid || rated.DurationFrames != 45 ||
		rated.Tracks[0].Clips[0].TimelineEndFrame != 15 ||
		rated.Tracks[2].Clips[0].TimelineEndFrame != 15 {
		t.Fatalf("rated=%#v audio=%#v report=%#v err=%v", rated.Tracks[0], rated.Tracks[2], Validate(rated), err)
	}

	faded, err := ApplyPatch(base, map[string]any{
		"kind": "set_clip_fades", "timeline_clip_id": "clip_v1_001",
		"fade_in_frames": 6, "fade_out_frames": 12,
	})
	if err != nil || faded.Tracks[0].Clips[0].FadeInFrames != 6 || faded.Tracks[0].Clips[0].FadeOutFrames != 12 ||
		faded.Tracks[2].Clips[0].FadeInFrames != 6 || faded.Tracks[2].Clips[0].FadeOutFrames != 12 {
		t.Fatalf("faded=%#v audio=%#v err=%v", faded.Tracks[0].Clips[0], faded.Tracks[2].Clips[0], err)
	}
	lockedAudio := base
	lockedAudio.Tracks = append([]Track(nil), base.Tracks...)
	lockedAudio.Tracks[2].Locked = true
	if _, err := ApplyPatch(lockedAudio, map[string]any{
		"kind": "set_clip_fades", "timeline_clip_id": "clip_v1_001",
		"fade_in_frames": 6, "fade_out_frames": 12,
	}); err == nil {
		t.Fatal("联动原声轨锁定时不应只更新画面淡化")
	}

	inserted, err := ApplyPatch(base, map[string]any{
		"kind": "insert_clip", "timeline_clip_id": "inserted_talk", "track_id": "visual_base",
		"asset_id": "talk", "asset_kind": "video", "source_start_frame": 100, "source_end_frame": 120,
		"include_original_audio": true,
	})
	if err != nil || !Validate(inserted).Valid || len(inserted.Tracks[0].Clips) != 3 || len(inserted.Tracks[2].Clips) != 3 {
		t.Fatalf("inserted=%#v audio=%#v report=%#v err=%v", inserted.Tracks[0], inserted.Tracks[2], Validate(inserted), err)
	}
}

func validationHasCode(report ValidationReport, code string) bool {
	for _, issue := range report.Issues {
		if issue.Code == code {
			return true
		}
	}
	return false
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
	base.Tracks[0].Clips[0].AssetKind = "video"
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
		{"insert bgm", map[string]any{
			"kind": "insert_clip", "track_id": "bgm", "timeline_clip_id": "bgm_1",
			"asset_id": "music", "asset_kind": "audio", "role": "bgm",
			"timeline_start_frame": 15, "source_start_frame": 0, "source_end_frame": 30,
		}, func(value Document) bool {
			return value.DurationFrames == 120 && len(value.Tracks[4].Clips) == 1 &&
				value.Tracks[4].Clips[0].TimelineStartFrame == 15 &&
				value.Tracks[4].Clips[0].TimelineEndFrame == 45 &&
				value.Tracks[4].Clips[0].AssetKind == "audio" && Validate(value).Valid
		}},
		{"replace", map[string]any{"kind": "replace_clip", "timeline_clip_id": "clip_v1_001", "asset_id": "c"}, func(value Document) bool { return value.Tracks[0].Clips[0].AssetID == "c" }},
		{"rate", map[string]any{"kind": "set_playback_rate", "timeline_clip_id": "clip_v1_001", "playback_rate": 2.0}, func(value Document) bool { return value.DurationFrames == 90 }},
		{"gain", map[string]any{"kind": "adjust_gain", "timeline_clip_id": "clip_v1_001", "gain_db": -3.0}, func(value Document) bool { return value.Tracks[0].Clips[0].GainDB == -3 }},
		{"fades", map[string]any{"kind": "set_clip_fades", "timeline_clip_id": "clip_v1_001", "fade_in_frames": 6, "fade_out_frames": 12}, func(value Document) bool {
			return value.Tracks[0].Clips[0].FadeInFrames == 6 && value.Tracks[0].Clips[0].FadeOutFrames == 12
		}},
		{"ducking", map[string]any{"kind": "set_track_ducking", "track_id": "bgm", "enabled": true, "duck_db": -9.0, "trigger_tracks": []string{"voiceover", "original_audio"}}, func(value Document) bool {
			return value.Tracks[4].Ducking != nil && value.Tracks[4].Ducking.Enabled && value.Tracks[4].Ducking.DuckDB == -9 && len(value.Tracks[4].Ducking.TriggerTracks) == 2
		}},
		{"subtitle", map[string]any{"kind": "edit_subtitle_text", "timeline_clip_id": "subtitle", "text": "新字幕", "style": "large_center"}, func(value Document) bool {
			return value.Tracks[5].Clips[0].Text == "新字幕" && value.Tracks[5].Clips[0].SubtitleStyle == "large_center"
		}},
		{"subtitle style only", map[string]any{"kind": "edit_subtitle_text", "timeline_clip_id": "subtitle", "style": "bold_bottom"}, func(value Document) bool {
			return value.Tracks[5].Clips[0].Text == "旧字幕" && value.Tracks[5].Clips[0].SubtitleStyle == "bold_bottom"
		}},
		{"insert subtitle", map[string]any{"kind": "insert_subtitle", "timeline_clip_id": "subtitle_new", "start_frame": 30, "end_frame": 60, "text": "新增字幕", "style": "top_bar"}, func(value Document) bool {
			return len(value.Tracks[5].Clips) == 2 && value.Tracks[5].Clips[1].Text == "新增字幕" && value.Tracks[5].Clips[1].SubtitleStyle == "top_bar"
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

func TestTrimAndPlaybackRateOnFreeTrackDoNotChangeCompositionDuration(t *testing.T) {
	t.Parallel()
	document, err := ComposeInitial("draft_free_audio", 1, []Selection{{
		AssetID: "video", AssetKind: "video", SourceEndFrame: 120,
	}})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Clips = []Clip{{
		TimelineClipID: "bgm", TrackID: "bgm", AssetID: "music", AssetKind: "audio",
		TimelineStartFrame: 0, TimelineEndFrame: 120, SourceEndFrame: 120, PlaybackRate: 1,
	}}

	trimmed, err := ApplyPatch(document, map[string]any{
		"kind": "trim_clip", "timeline_clip_id": "bgm",
		"source_start_frame": 0, "source_end_frame": 60,
	})
	if err != nil || trimmed.DurationFrames != 120 || trimmed.Tracks[4].Clips[0].TimelineEndFrame != 60 {
		t.Fatalf("trimmed=%#v err=%v", trimmed, err)
	}
	rated, err := ApplyPatch(document, map[string]any{
		"kind": "set_playback_rate", "timeline_clip_id": "bgm", "playback_rate": 2.0,
	})
	if err != nil || rated.DurationFrames != 120 || rated.Tracks[4].Clips[0].TimelineEndFrame != 60 {
		t.Fatalf("rated=%#v err=%v", rated, err)
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
		AssetKind:          "audio",
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
		{"kind": "set_clip_fades", "timeline_clip_id": "clip_v1_001", "fade_in_frames": 40, "fade_out_frames": 40},
		{"kind": "set_track_ducking", "track_id": "voiceover", "enabled": true, "duck_db": -9.0, "trigger_tracks": []string{"voiceover"}},
		{"kind": "set_track_ducking", "track_id": "bgm", "enabled": true, "duck_db": -30.0, "trigger_tracks": []string{"voiceover"}},
		{"kind": "set_track_ducking", "track_id": "bgm", "enabled": true, "duck_db": -9.0, "trigger_tracks": []string{"sfx"}},
		{"kind": "set_track_ducking", "track_id": "bgm", "duck_db": -9.0, "trigger_tracks": []string{"voiceover"}},
		{"kind": "set_track_ducking", "track_id": "bgm", "enabled": true, "duck_db": -9.0, "trigger_tracks": "voiceover"},
		{"kind": "set_track_ducking", "track_id": "bgm", "enabled": true, "duck_db": -9.0, "trigger_tracks": []string{}},
		{"kind": "edit_subtitle_text", "timeline_clip_id": "missing", "text": "x"},
		{"kind": "edit_subtitle_text", "timeline_clip_id": "clip_v1_001", "text": "x", "style": "karaoke"},
		{"kind": "edit_subtitle_text", "timeline_clip_id": "subtitle"},
		{"kind": "edit_subtitle_text", "timeline_clip_id": "subtitle", "text": " ", "style": "default"},
		{"kind": "insert_subtitle", "start_frame": 0, "end_frame": 99, "text": ""},
		{"kind": "insert_subtitle", "start_frame": 0, "end_frame": 30, "text": "x", "style": "karaoke"},
		{"kind": "remove_track_clips", "track_id": "visual_base"},
	}
	ducked, err := ApplyPatch(base, map[string]any{
		"kind": "set_track_ducking", "track_id": "bgm", "enabled": false,
		"duck_db": -6.0, "trigger_tracks": []any{"voiceover", "voiceover"},
	})
	if err != nil || ducked.Tracks[4].Ducking == nil || ducked.Tracks[4].Ducking.Enabled ||
		len(ducked.Tracks[4].Ducking.TriggerTracks) != 1 {
		t.Fatalf("generic deduplicated triggers=%#v err=%v", ducked.Tracks[4].Ducking, err)
	}
	locked := base
	locked.Tracks[4].Locked = true
	if _, err := ApplyPatch(locked, map[string]any{
		"kind": "set_track_ducking", "track_id": "bgm", "enabled": true,
		"duck_db": -9.0, "trigger_tracks": []string{"voiceover"},
	}); err == nil {
		t.Fatal("locked bgm track should reject ducking")
	}
	for _, operation := range badOperations {
		if _, err := ApplyPatch(base, operation); err == nil {
			t.Fatalf("operation should fail: %#v", operation)
		}
	}
	for index, selection := range []Selection{
		{},
		{AssetID: "a", SourceStartFrame: -1, SourceEndFrame: 1},
		{AssetID: "a", SourceStartFrame: 2, SourceEndFrame: 1},
		{AssetID: "a", AssetKind: "audio", SourceEndFrame: 1},
	} {
		if _, err := ComposeInitial("draft", 1, []Selection{selection}); err == nil {
			t.Fatalf("selection[%d] should fail", index)
		}
	}
	if _, err := ComposeInitial("", 1, []Selection{{AssetID: "a", SourceEndFrame: 1}}); err == nil {
		t.Fatal("empty draft should fail")
	}
}

func TestValidatePresentationFields(t *testing.T) {
	t.Parallel()
	base, err := ComposeInitial("draft_presentation_validation", 1, []Selection{{AssetID: "a", SourceEndFrame: 60}})
	if err != nil {
		t.Fatal(err)
	}
	subtitles := trackByID(&base, "subtitles")
	subtitles.Clips = []Clip{{
		TimelineClipID: "subtitle", TrackID: "subtitles", Text: "字幕",
		TimelineStartFrame: 0, TimelineEndFrame: 30,
	}}
	bgm := trackByID(&base, "bgm")
	bgm.Ducking = &TrackDucking{Enabled: true, DuckDB: -9, TriggerTracks: []string{"voiceover", "original_audio"}}
	if report := Validate(base); !report.Valid {
		t.Fatalf("valid presentation fields: %#v", report.Issues)
	}

	tests := []struct {
		name string
		edit func(*Document)
		code string
	}{
		{name: "ducking only on bgm", code: "invalid_track_ducking", edit: func(document *Document) {
			document.Tracks[0].Ducking = document.Tracks[4].Ducking
		}},
		{name: "duck db range", code: "invalid_duck_db", edit: func(document *Document) {
			document.Tracks[4].Ducking.DuckDB = 20
		}},
		{name: "unknown trigger", code: "invalid_ducking_trigger", edit: func(document *Document) {
			document.Tracks[4].Ducking.TriggerTracks = []string{"sfx"}
		}},
		{name: "empty triggers", code: "empty_ducking_triggers", edit: func(document *Document) {
			document.Tracks[4].Ducking.TriggerTracks = nil
		}},
		{name: "duplicate triggers", code: "duplicate_ducking_trigger", edit: func(document *Document) {
			document.Tracks[4].Ducking.TriggerTracks = []string{"voiceover", "voiceover"}
		}},
		{name: "unknown subtitle style", code: "invalid_subtitle_style", edit: func(document *Document) {
			document.Tracks[5].Clips[0].SubtitleStyle = "karaoke"
		}},
		{name: "subtitle style only on subtitle track", code: "invalid_subtitle_style_track", edit: func(document *Document) {
			document.Tracks[0].Clips[0].SubtitleStyle = "bold_bottom"
		}},
		{name: "unknown subtitle style on non-subtitle track", code: "invalid_subtitle_style_track", edit: func(document *Document) {
			document.Tracks[0].Clips[0].SubtitleStyle = "karaoke"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document, cloneErr := clone(base)
			if cloneErr != nil {
				t.Fatal(cloneErr)
			}
			test.edit(&document)
			report := Validate(document)
			if report.Valid || !hasValidationIssue(report, test.code) {
				t.Fatalf("report=%#v", report)
			}
		})
	}
}

func hasValidationIssue(report ValidationReport, code string) bool {
	for _, issue := range report.Issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func TestDeleteRangeHandlesEveryOverlapShape(t *testing.T) {
	t.Parallel()
	document := Empty("draft_overlap", 1)
	document.DurationFrames = 120
	document.Tracks[0].Clips = []Clip{{
		TimelineClipID: "cover", TrackID: "visual_base", AssetID: "a",
		TimelineStartFrame: 0, TimelineEndFrame: 120, SourceStartFrame: 0, SourceEndFrame: 120, PlaybackRate: 1,
		ParentBlockID: "block_cover", Linked: true,
	}}
	document.Tracks[2].Clips = []Clip{{
		TimelineClipID: "cover_audio", TrackID: "original_audio", AssetID: "a",
		TimelineStartFrame: 0, TimelineEndFrame: 120, SourceStartFrame: 0, SourceEndFrame: 120, PlaybackRate: 1,
		ParentBlockID: "block_cover", Linked: true,
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
	for _, trackIndex := range []int{0, 2} {
		clips := result.Tracks[trackIndex].Clips
		if len(clips) != 2 || clips[0].TimelineStartFrame != 0 || clips[0].TimelineEndFrame != 30 ||
			clips[0].SourceStartFrame != 0 || clips[0].SourceEndFrame != 30 ||
			clips[1].TimelineStartFrame != 30 || clips[1].TimelineEndFrame != 90 ||
			clips[1].SourceStartFrame != 60 || clips[1].SourceEndFrame != 120 {
			t.Fatalf("中段删除没有保留正确源区间，track=%s clips=%#v", result.Tracks[trackIndex].TrackID, clips)
		}
		if !clips[0].Linked || !clips[1].Linked || clips[0].ParentBlockID == clips[1].ParentBlockID {
			t.Fatalf("A/V 联动分段无效，track=%s clips=%#v", result.Tracks[trackIndex].TrackID, clips)
		}
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
			timeline_current_version,timeline_validated,created_at,updated_at)
		VALUES('draft_preview','d',0,'active','{}','[]','{}',1,1,?,?);
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

func TestNextVersionFollowsOnlyCurrentTimeline(t *testing.T) {
	t.Parallel()
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO drafts(
			draft_id,name,state_version,status,defaults_json,running_jobs_json,brief_json,
			timeline_current_version,timeline_validated,created_at,updated_at
			) VALUES('draft_nav','d',0,'active','{}','[]','{}',4,1,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	version, err := NextVersion(t.Context(), database, "draft_nav")
	if err != nil || version != 5 {
		t.Fatalf("next version=%d err=%v", version, err)
	}
}
