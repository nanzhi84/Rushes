package timeline

import (
	"math"
	"strings"
	"testing"
)

func newEditingDocument(t *testing.T, linked bool) Document {
	t.Helper()
	document, err := ComposeInitial("draft_editing_branches", 1, []Selection{
		{AssetID: "a", AssetKind: "video", SourceEndFrame: 30, Role: "a_roll", HasAudio: linked},
		{AssetID: "b", AssetKind: "video", SourceEndFrame: 30, Role: "a_roll", HasAudio: linked},
		{AssetID: "c", AssetKind: "video", SourceEndFrame: 30, Role: "a_roll", HasAudio: linked},
	})
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func TestTrackStateAndClipLinkingBranches(t *testing.T) {
	document := newEditingDocument(t, true)
	if err := setClipLinked(&document, map[string]any{
		"timeline_clip_id": "clip_v1_001", "linked": false,
	}); err != nil {
		t.Fatal(err)
	}
	if document.Tracks[0].Clips[0].Linked || document.Tracks[2].Clips[0].Linked {
		t.Fatal("取消联动后不应留下单独的联动成员")
	}
	if err := setClipLinked(&document, map[string]any{
		"timeline_clip_id": "clip_v1_001", "linked": true,
	}); err != nil {
		t.Fatal(err)
	}
	groupID := document.Tracks[0].Clips[0].ParentBlockID
	if groupID != "link_clip_v1_001" || document.Tracks[2].Clips[0].ParentBlockID != groupID {
		t.Fatalf("group id=%q visual=%#v audio=%#v", groupID, document.Tracks[0].Clips[0], document.Tracks[2].Clips[0])
	}
	if err := setClipLinked(&document, map[string]any{
		"timeline_clip_id": "clip_v1_001", "linked": true,
	}); err != nil {
		t.Fatalf("重复联动应保持幂等: %v", err)
	}
	if candidate, found := linkCandidate(&document, clipLocation{trackIndex: 2, clipIndex: 0}); !found || candidate.trackIndex != 0 {
		t.Fatalf("audio candidate=%#v found=%v", candidate, found)
	}

	if err := setClipLinked(&document, map[string]any{
		"timeline_clip_id": "clip_v1_001_audio", "linked": false,
	}); err != nil {
		t.Fatal(err)
	}
	document.Tracks[0].Clips[0].ParentBlockID = "visual_group"
	if err := setClipLinked(&document, map[string]any{
		"timeline_clip_id": "clip_v1_001_audio", "linked": true,
	}); err != nil {
		t.Fatal(err)
	}
	if document.Tracks[2].Clips[0].ParentBlockID != "visual_group" {
		t.Fatalf("应复用候选片段的 group: %#v", document.Tracks[2].Clips[0])
	}

	if err := setClipLinked(&document, map[string]any{
		"timeline_clip_id": "clip_v1_002", "linked": false,
	}); err != nil {
		t.Fatal(err)
	}
	document.Tracks[2].Clips[1].ParentBlockID = "audio_group"
	if err := setClipLinked(&document, map[string]any{
		"timeline_clip_id": "clip_v1_002", "linked": true,
	}); err != nil {
		t.Fatal(err)
	}
	if document.Tracks[0].Clips[1].ParentBlockID != "audio_group" {
		t.Fatalf("应复用音频候选的 group: %#v", document.Tracks[0].Clips[1])
	}

	if err := setClipLinked(&document, map[string]any{"timeline_clip_id": "clip_v1_003"}); err == nil {
		t.Fatal("缺少 linked 应失败")
	}
	if err := setClipLinked(&document, map[string]any{
		"timeline_clip_id": "clip_v1_003", "linked": false,
	}); err != nil {
		t.Fatal(err)
	}
	document.Tracks[2].Locked = true
	if err := setClipLinked(&document, map[string]any{
		"timeline_clip_id": "clip_v1_003", "linked": true,
	}); err == nil || !strings.Contains(err.Error(), "已锁定") {
		t.Fatalf("锁定候选应失败: %v", err)
	}
	document.Tracks[2].Locked = false
	document.Tracks[2].Clips[2].AssetID = "mismatch"
	if err := setClipLinked(&document, map[string]any{
		"timeline_clip_id": "clip_v1_003", "linked": true,
	}); err == nil || !strings.Contains(err.Error(), "没有可") {
		t.Fatalf("无同源候选应失败: %v", err)
	}
	document.Tracks[1].Clips = []Clip{{
		TimelineClipID: "overlay_link", TrackID: "visual_overlay", AssetID: "a",
		TimelineStartFrame: 0, TimelineEndFrame: 30, SourceEndFrame: 30,
	}}
	if _, found := linkCandidate(&document, clipLocation{trackIndex: 1, clipIndex: 0}); found {
		t.Fatal("叠加轨不应参与原始音画联动")
	}

	stateErrors := []map[string]any{
		{"track_id": "missing", "muted": true},
		{"track_id": "voiceover", "muted": "yes"},
		{"track_id": "voiceover", "gain_db": math.NaN()},
		{"track_id": "voiceover", "gain_db": 13.0},
		{"track_id": "visual_overlay", "gain_db": -2.0},
		{"track_id": "voiceover"},
	}
	for _, operation := range stateErrors {
		copy := document
		if err := setTrackState(&copy, operation); err == nil {
			t.Fatalf("setTrackState 应失败: %#v", operation)
		}
	}
	if err := setTrackState(&document, map[string]any{
		"track_id": "voiceover", "muted": true, "solo": true, "locked": true, "gain_db": float32(-4),
	}); err != nil {
		t.Fatal(err)
	}
	if !document.Tracks[3].Muted || !document.Tracks[3].Solo || !document.Tracks[3].Locked || document.Tracks[3].GainDB != -4 {
		t.Fatalf("track state=%#v", document.Tracks[3])
	}
}

func TestMoveClipInsertOverwriteAndFailureBranches(t *testing.T) {
	t.Run("unlinked primary reorders on its own track", func(t *testing.T) {
		document := newEditingDocument(t, false)
		if err := moveClip(&document, map[string]any{
			"timeline_clip_id": "clip_v1_001", "target_track_id": "visual_base", "target_frame": 90,
		}); err != nil {
			t.Fatal(err)
		}
		if document.Tracks[0].Clips[2].TimelineClipID != "clip_v1_001" {
			t.Fatalf("primary clips=%#v", document.Tracks[0].Clips)
		}
	})

	t.Run("overlay overwrites primary", func(t *testing.T) {
		document := newEditingDocument(t, false)
		document.Tracks[1].Clips = []Clip{{
			TimelineClipID: "overlay", TrackID: "visual_overlay", AssetID: "still", AssetKind: "image",
			TimelineStartFrame: 0, TimelineEndFrame: 15, SourceStartFrame: 0, SourceEndFrame: 15, PlaybackRate: 1,
		}}
		if err := moveClip(&document, map[string]any{
			"timeline_clip_id": "overlay", "target_track_id": "visual_base", "target_frame": 15, "mode": "overwrite",
		}); err != nil {
			t.Fatal(err)
		}
		if len(document.Tracks[1].Clips) != 0 || document.Tracks[0].Clips[1].TimelineClipID != "overlay" {
			t.Fatalf("tracks=%#v", document.Tracks[:2])
		}
		if report := Validate(document); !report.Valid {
			t.Fatalf("report=%#v", report)
		}
	})

	t.Run("primary moves to overlay with ripple and clamping", func(t *testing.T) {
		document := newEditingDocument(t, false)
		if err := moveClip(&document, map[string]any{
			"timeline_clip_id": "clip_v1_001", "target_track_id": "visual_overlay", "target_frame": 90, "mode": "insert",
		}); err != nil {
			t.Fatal(err)
		}
		if document.DurationFrames != 60 || len(document.Tracks[0].Clips) != 2 || len(document.Tracks[1].Clips) != 1 {
			t.Fatalf("document=%#v", document)
		}
		moved := document.Tracks[1].Clips[0]
		if moved.TimelineStartFrame != 30 || moved.TimelineEndFrame != 60 || moved.TrackID != "visual_overlay" {
			t.Fatalf("moved=%#v", moved)
		}
	})

	t.Run("same free track supports insert and overwrite", func(t *testing.T) {
		for _, mode := range []string{"insert", "overwrite"} {
			document := newEditingDocument(t, false)
			document.Tracks[1].Clips = []Clip{
				{TimelineClipID: "moving", TrackID: "visual_overlay", AssetID: "x", AssetKind: "video", TimelineStartFrame: 0, TimelineEndFrame: 20, SourceEndFrame: 20, PlaybackRate: 1},
				{TimelineClipID: "occupied", TrackID: "visual_overlay", AssetID: "y", AssetKind: "video", TimelineStartFrame: 15, TimelineEndFrame: 50, SourceEndFrame: 35, PlaybackRate: 1},
			}
			if err := moveClip(&document, map[string]any{
				"timeline_clip_id": "moving", "target_frame": 25, "mode": mode,
			}); err != nil {
				t.Fatalf("mode=%s err=%v", mode, err)
			}
			if len(document.Tracks[1].Clips) == 0 {
				t.Fatalf("mode=%s clips=%#v", mode, document.Tracks[1].Clips)
			}
		}
	})

	t.Run("linked and invalid moves are rejected", func(t *testing.T) {
		linked := newEditingDocument(t, true)
		if err := moveClip(&linked, map[string]any{
			"timeline_clip_id": "clip_v1_001", "target_track_id": "visual_overlay", "target_frame": 0,
		}); err == nil || !strings.Contains(err.Error(), "取消音画联动") {
			t.Fatalf("linked cross-track err=%v", err)
		}

		freeGroup := newEditingDocument(t, false)
		freeGroup.Tracks[3].Clips = []Clip{{
			TimelineClipID: "voice_linked", TrackID: "voiceover", AssetID: "audio", AssetKind: "audio",
			TimelineStartFrame: 0, TimelineEndFrame: 20, SourceEndFrame: 20, Linked: true, ParentBlockID: "free_group",
		}}
		freeGroup.Tracks[4].Clips = []Clip{{
			TimelineClipID: "bgm_linked", TrackID: "bgm", AssetID: "audio", AssetKind: "audio",
			TimelineStartFrame: 0, TimelineEndFrame: 20, SourceEndFrame: 20, Linked: true, ParentBlockID: "free_group",
		}}
		if err := moveClip(&freeGroup, map[string]any{
			"timeline_clip_id": "voice_linked", "target_track_id": "bgm", "target_frame": 10,
		}); err == nil || !strings.Contains(err.Error(), "取消片段联动") {
			t.Fatalf("free linked cross-track err=%v", err)
		}

		tests := []struct {
			name    string
			prepare func(*Document)
			op      map[string]any
		}{
			{"missing clip", nil, map[string]any{"timeline_clip_id": "missing", "target_frame": 0}},
			{"invalid frame", nil, map[string]any{"timeline_clip_id": "clip_v1_001", "target_frame": "0"}},
			{"invalid mode", nil, map[string]any{"timeline_clip_id": "clip_v1_001", "target_frame": 0, "mode": "replace"}},
			{"missing target", nil, map[string]any{"timeline_clip_id": "clip_v1_001", "target_frame": 0, "target_track_id": "missing"}},
			{"locked target", func(value *Document) { value.Tracks[1].Locked = true }, map[string]any{"timeline_clip_id": "clip_v1_001", "target_frame": 0, "target_track_id": "visual_overlay"}},
			{"incompatible target", nil, map[string]any{"timeline_clip_id": "clip_v1_001", "target_frame": 0, "target_track_id": "voiceover"}},
			{"invalid duration", func(value *Document) {
				value.Tracks[1].Clips = []Clip{{TimelineClipID: "bad", TrackID: "visual_overlay", AssetID: "x", TimelineStartFrame: 5, TimelineEndFrame: 5}}
			}, map[string]any{"timeline_clip_id": "bad", "target_frame": 0}},
			{"too long", func(value *Document) {
				value.Tracks[1].Clips = []Clip{{TimelineClipID: "long", TrackID: "visual_overlay", AssetID: "x", AssetKind: "image", TimelineStartFrame: 0, TimelineEndFrame: 100, SourceEndFrame: 100}}
			}, map[string]any{"timeline_clip_id": "long", "target_frame": 0}},
		}
		for _, item := range tests {
			document := newEditingDocument(t, false)
			if item.prepare != nil {
				item.prepare(&document)
			}
			if err := moveClip(&document, item.op); err == nil {
				t.Fatalf("%s 应失败", item.name)
			}
		}

		single, err := ComposeInitial("single", 1, []Selection{{AssetID: "a", AssetKind: "video", SourceEndFrame: 30}})
		if err != nil {
			t.Fatal(err)
		}
		if err := moveClip(&single, map[string]any{
			"timeline_clip_id": "clip_v1_001", "target_track_id": "visual_overlay", "target_frame": 0,
		}); err == nil || !strings.Contains(err.Error(), "至少保留") {
			t.Fatalf("single primary err=%v", err)
		}
	})

	t.Run("primary insertion helpers reject unsafe targets", func(t *testing.T) {
		moving := Clip{TimelineClipID: "moving", TrackID: "visual_overlay", AssetID: "x", TimelineStartFrame: 0, TimelineEndFrame: 10, SourceEndFrame: 10}
		missing := newEditingDocument(t, false)
		missing.Tracks = missing.Tracks[1:]
		if err := insertIntoPrimary(&missing, moving, 0); err == nil {
			t.Fatal("missing primary should fail")
		}
		locked := newEditingDocument(t, false)
		locked.Tracks[0].Locked = true
		if err := insertIntoPrimary(&locked, moving, 0); err == nil {
			t.Fatal("locked primary should fail")
		}
		rippleLocked := newEditingDocument(t, false)
		rippleLocked.Tracks[3].Locked = true
		rippleLocked.Tracks[3].Clips = []Clip{{TimelineClipID: "locked", TimelineEndFrame: 20}}
		if err := insertIntoPrimary(&rippleLocked, moving, 0); err == nil || !strings.Contains(err.Error(), "波纹") {
			t.Fatalf("ripple lock err=%v", err)
		}
		unsplittable := newEditingDocument(t, false)
		unsplittable.Tracks[0].Clips[0].AssetID = ""
		if err := insertIntoPrimary(&unsplittable, moving, 5); err == nil {
			t.Fatal("unsplittable primary should reject an interior insertion")
		}

		if err := overwritePrimary(&missing, moving, 0); err == nil {
			t.Fatal("missing primary overwrite should fail")
		}
		locked = newEditingDocument(t, false)
		locked.Tracks[0].Locked = true
		if err := overwritePrimary(&locked, moving, 0); err == nil {
			t.Fatal("locked primary overwrite should fail")
		}
		tooLong := newEditingDocument(t, false)
		moving.TimelineEndFrame = tooLong.DurationFrames + 1
		if err := overwritePrimary(&tooLong, moving, 0); err == nil {
			t.Fatal("oversized primary overwrite should fail")
		}
	})
}

func TestTrimDeleteSubtitleAndRippleBranches(t *testing.T) {
	t.Run("primary and free trim edges", func(t *testing.T) {
		primary := newEditingDocument(t, false)
		if err := trimClipEdge(&primary, map[string]any{
			"timeline_clip_id": "clip_v1_001", "edge": "end", "timeline_frame": 15,
		}); err != nil {
			t.Fatal(err)
		}
		if primary.DurationFrames != 75 || primary.Tracks[0].Clips[0].TimelineEndFrame != 15 {
			t.Fatalf("primary=%#v", primary)
		}

		free := newEditingDocument(t, false)
		free.Tracks[3].Clips = []Clip{{
			TimelineClipID: "voice", TrackID: "voiceover", AssetID: "voice", AssetKind: "audio",
			TimelineStartFrame: 10, TimelineEndFrame: 40, SourceStartFrame: 0, SourceEndFrame: 60, PlaybackRate: 2,
		}}
		if err := trimClipEdge(&free, map[string]any{
			"timeline_clip_id": "voice", "edge": "end", "timeline_frame": 30,
		}); err != nil {
			t.Fatal(err)
		}
		if free.Tracks[3].Clips[0].TimelineEndFrame != 30 || free.Tracks[3].Clips[0].SourceEndFrame != 40 {
			t.Fatalf("voice=%#v", free.Tracks[3].Clips[0])
		}
		free.Tracks[5].Clips = []Clip{{
			TimelineClipID: "subtitle_free", TrackID: "subtitles", Text: "text", TimelineStartFrame: 0, TimelineEndFrame: 20,
		}}
		if err := trimClipEdge(&free, map[string]any{
			"timeline_clip_id": "subtitle_free", "edge": "start", "timeline_frame": 5,
		}); err != nil {
			t.Fatal(err)
		}

		free.Tracks[3].Clips[0].Linked = true
		free.Tracks[3].Clips[0].ParentBlockID = "free_trim"
		free.Tracks[4].Clips = []Clip{{
			TimelineClipID: "short_partner", TrackID: "bgm", AssetID: "voice", AssetKind: "audio",
			TimelineStartFrame: 0, TimelineEndFrame: 20, SourceEndFrame: 20, Linked: true, ParentBlockID: "free_trim",
		}}
		if err := trimClipEdge(&free, map[string]any{
			"timeline_clip_id": "voice", "edge": "end", "timeline_frame": 25,
		}); err != nil {
			t.Fatal(err)
		}
		if free.Tracks[4].Clips[0].TimelineEndFrame != 20 {
			t.Fatalf("uncovered partner should remain unchanged: %#v", free.Tracks[4].Clips[0])
		}
	})

	t.Run("trim failures", func(t *testing.T) {
		operations := []map[string]any{
			{"timeline_clip_id": "missing", "edge": "start", "timeline_frame": 5},
			{"timeline_clip_id": "clip_v1_001", "edge": "start", "timeline_frame": "5"},
			{"timeline_clip_id": "clip_v1_001", "edge": "middle", "timeline_frame": 5},
			{"timeline_clip_id": "clip_v1_001", "edge": "start", "timeline_frame": 0},
		}
		for _, operation := range operations {
			document := newEditingDocument(t, false)
			if err := trimClipEdge(&document, operation); err == nil {
				t.Fatalf("trim 应失败: %#v", operation)
			}
		}
		linked := newEditingDocument(t, true)
		linked.Tracks[2].Locked = true
		if err := trimClipEdge(&linked, map[string]any{
			"timeline_clip_id": "clip_v1_001", "edge": "start", "timeline_frame": 5,
		}); err == nil || !strings.Contains(err.Error(), "已锁定") {
			t.Fatalf("locked member err=%v", err)
		}
		rippleLocked := newEditingDocument(t, false)
		rippleLocked.Tracks[3].Locked = true
		rippleLocked.Tracks[3].Clips = []Clip{{TimelineClipID: "lock", TimelineEndFrame: 90}}
		if err := trimClipEdge(&rippleLocked, map[string]any{
			"timeline_clip_id": "clip_v1_001", "edge": "end", "timeline_frame": 15,
		}); err == nil || !strings.Contains(err.Error(), "波纹") {
			t.Fatalf("ripple lock err=%v", err)
		}
	})

	t.Run("delete primary and linked free groups", func(t *testing.T) {
		primary := newEditingDocument(t, true)
		if err := deleteClip(&primary, map[string]any{"timeline_clip_id": "clip_v1_001"}); err != nil {
			t.Fatal(err)
		}
		if primary.DurationFrames != 60 || len(primary.Tracks[0].Clips) != 2 || len(primary.Tracks[2].Clips) != 2 {
			t.Fatalf("primary=%#v", primary)
		}

		free := newEditingDocument(t, false)
		free.Tracks[3].Clips = []Clip{
			{TimelineClipID: "voice_linked", TrackID: "voiceover", AssetID: "voice", TimelineEndFrame: 20, SourceEndFrame: 20, Linked: true, ParentBlockID: "free_delete"},
			{TimelineClipID: "voice_keep", TrackID: "voiceover", AssetID: "voice", TimelineStartFrame: 30, TimelineEndFrame: 50, SourceEndFrame: 20},
		}
		free.Tracks[4].Clips = []Clip{{
			TimelineClipID: "bgm_linked", TrackID: "bgm", AssetID: "voice", TimelineEndFrame: 20, SourceEndFrame: 20, Linked: true, ParentBlockID: "free_delete",
		}}
		if err := deleteClip(&free, map[string]any{"timeline_clip_id": "voice_linked"}); err != nil {
			t.Fatal(err)
		}
		if len(free.Tracks[3].Clips) != 1 || free.Tracks[3].Clips[0].TimelineClipID != "voice_keep" || len(free.Tracks[4].Clips) != 0 {
			t.Fatalf("free tracks=%#v %#v", free.Tracks[3], free.Tracks[4])
		}
	})

	t.Run("delete failures", func(t *testing.T) {
		missing := newEditingDocument(t, false)
		if err := deleteClip(&missing, map[string]any{"timeline_clip_id": "missing"}); err == nil {
			t.Fatal("missing clip should fail deletion")
		}
		single, err := ComposeInitial("single_delete", 1, []Selection{{AssetID: "a", SourceEndFrame: 30}})
		if err != nil {
			t.Fatal(err)
		}
		if err := deleteClip(&single, map[string]any{"timeline_clip_id": "clip_v1_001"}); err == nil {
			t.Fatal("last primary should not be deleted")
		}
		linked := newEditingDocument(t, true)
		linked.Tracks[2].Locked = true
		if err := deleteClip(&linked, map[string]any{"timeline_clip_id": "clip_v1_001"}); err == nil {
			t.Fatal("locked linked member should block deletion")
		}
		rippleLocked := newEditingDocument(t, false)
		rippleLocked.Tracks[3].Locked = true
		rippleLocked.Tracks[3].Clips = []Clip{{TimelineClipID: "locked", TimelineEndFrame: 90}}
		if err := deleteClip(&rippleLocked, map[string]any{"timeline_clip_id": "clip_v1_001"}); err == nil {
			t.Fatal("locked ripple track should block deletion")
		}
	})

	t.Run("subtitle validation and generated ids", func(t *testing.T) {
		missing := newEditingDocument(t, false)
		missing.Tracks = missing.Tracks[:5]
		if err := insertSubtitle(&missing, map[string]any{"start_frame": 0, "end_frame": 10, "text": "x"}); err == nil {
			t.Fatal("missing subtitle track should fail")
		}
		locked := newEditingDocument(t, false)
		locked.Tracks[5].Locked = true
		if err := insertSubtitle(&locked, map[string]any{"start_frame": 0, "end_frame": 10, "text": "x"}); err == nil {
			t.Fatal("locked subtitle track should fail")
		}
		invalid := newEditingDocument(t, false)
		if err := insertSubtitle(&invalid, map[string]any{"start_frame": "0", "end_frame": 10, "text": "x"}); err == nil {
			t.Fatal("invalid subtitle frame should fail")
		}
		if err := insertSubtitle(&invalid, map[string]any{"start_frame": 0, "end_frame": 100, "text": "x"}); err == nil {
			t.Fatal("out-of-range subtitle should fail")
		}
		invalid.Tracks[5].Clips = []Clip{{TimelineClipID: "duplicate", TrackID: "subtitles", Text: "old", TimelineEndFrame: 10}}
		if err := insertSubtitle(&invalid, map[string]any{"timeline_clip_id": "duplicate", "start_frame": 20, "end_frame": 30, "text": "x"}); err == nil {
			t.Fatal("duplicate subtitle id should fail")
		}
		if err := insertSubtitle(&invalid, map[string]any{"start_frame": 40, "end_frame": 50, "text": " generated "}); err != nil {
			t.Fatal(err)
		}
		if invalid.Tracks[5].Clips[1].TimelineClipID != "subtitle_v2_002" || invalid.Tracks[5].Clips[1].Text != "generated" {
			t.Fatalf("subtitles=%#v", invalid.Tracks[5].Clips)
		}
	})
}

func TestEditingRangeAndTypeHelpers(t *testing.T) {
	missing := newEditingDocument(t, false)
	if err := setClipLinked(&missing, map[string]any{"timeline_clip_id": "missing", "linked": true}); err == nil {
		t.Fatal("missing clip should fail linking")
	}

	track := Track{TrackID: "visual_overlay", Clips: []Clip{
		{TimelineClipID: "before", TimelineStartFrame: 0, TimelineEndFrame: 10},
		{TimelineClipID: "span", AssetID: "a", TimelineStartFrame: 5, TimelineEndFrame: 60, SourceStartFrame: 0, SourceEndFrame: 110, PlaybackRate: 2},
		{TimelineClipID: "inside", TimelineStartFrame: 25, TimelineEndFrame: 35},
		{TimelineClipID: "left", AssetID: "a", TimelineStartFrame: 10, TimelineEndFrame: 30, SourceEndFrame: 20},
		{TimelineClipID: "right", AssetID: "a", TimelineStartFrame: 30, TimelineEndFrame: 50, SourceEndFrame: 20},
		{TimelineClipID: "after", TimelineStartFrame: 50, TimelineEndFrame: 60},
	}}
	eraseTrackRange(&track, 20, 40)
	if len(track.Clips) != 6 {
		t.Fatalf("erase clips=%#v", track.Clips)
	}
	if track.Clips[1].TimelineClipID != "span" || track.Clips[2].TimelineClipID != "span_after_40" {
		t.Fatalf("spanning clip should split: %#v", track.Clips)
	}

	shifted := Track{Clips: []Clip{
		{TimelineClipID: "truncate_without_asset", TimelineStartFrame: 0, TimelineEndFrame: 95},
		{TimelineClipID: "shift_and_truncate", AssetID: "a", TimelineStartFrame: 20, TimelineEndFrame: 70, SourceEndFrame: 50, PlaybackRate: 1},
		{TimelineClipID: "drop", TimelineStartFrame: 70, TimelineEndFrame: 80},
	}}
	shiftTrackForInsert(&shifted, 20, 30, 90)
	if len(shifted.Clips) != 2 || shifted.Clips[0].TimelineEndFrame != 90 || shifted.Clips[1].TimelineEndFrame != 90 || shifted.Clips[1].SourceEndFrame != 40 {
		t.Fatalf("shifted=%#v", shifted.Clips)
	}

	document := newEditingDocument(t, false)
	if err := splitDocumentPrimaryAt(&document, 0); err != nil {
		t.Fatal(err)
	}
	if err := splitDocumentPrimaryAt(&document, 30); err != nil {
		t.Fatal(err)
	}
	if err := splitDocumentPrimaryAt(&document, 15); err != nil || len(document.Tracks[0].Clips) != 4 {
		t.Fatalf("split err=%v clips=%#v", err, document.Tracks[0].Clips)
	}
	if err := splitDocumentPrimaryAt(&document, 120); err != nil {
		t.Fatal(err)
	}
	missingPrimary := document
	missingPrimary.Tracks = missingPrimary.Tracks[1:]
	if err := splitDocumentPrimaryAt(&missingPrimary, 10); err == nil {
		t.Fatal("missing primary split should fail")
	}

	locked := newEditingDocument(t, false)
	locked.Tracks[3].Locked = true
	locked.Tracks[3].Clips = []Clip{{TimelineClipID: "before", TimelineEndFrame: 10}}
	if err := ensureRippleUnlocked(&locked, 10, ""); err != nil {
		t.Fatalf("clip ending at boundary should not block: %v", err)
	}
	locked.Tracks[3].Clips[0].TimelineEndFrame = 11
	if err := ensureRippleUnlocked(&locked, 10, ""); err == nil {
		t.Fatal("locked clip past boundary should block")
	}
	if err := ensureRippleUnlocked(&locked, 10, "voiceover"); err != nil {
		t.Fatalf("excepted track should not block: %v", err)
	}

	visualBase := Track{TrackID: "visual_base", TrackType: "primary_visual"}
	visualOverlay := Track{TrackID: "visual_overlay", TrackType: "visual_overlay"}
	audio := Track{TrackID: "voiceover", TrackType: "audio"}
	textTrack := Track{TrackID: "subtitles", TrackType: "text"}
	custom := Track{TrackID: "custom", TrackType: "custom"}
	if !tracksCompatible(visualBase, visualOverlay, Clip{AssetID: "x", AssetKind: "image"}) ||
		tracksCompatible(visualBase, visualOverlay, Clip{AssetKind: "image"}) ||
		!tracksCompatible(audio, Track{TrackID: "bgm", TrackType: "audio"}, Clip{AssetID: "x", AssetKind: "audio"}) ||
		tracksCompatible(audio, Track{TrackID: "bgm", TrackType: "audio"}, Clip{AssetID: "x", AssetKind: "image"}) ||
		!tracksCompatible(textTrack, textTrack, Clip{Text: "caption"}) ||
		tracksCompatible(textTrack, textTrack, Clip{Text: " "}) ||
		!tracksCompatible(custom, custom, Clip{}) ||
		tracksCompatible(custom, Track{TrackID: "other", TrackType: "other"}, Clip{}) ||
		tracksCompatible(visualBase, audio, Clip{AssetID: "x", AssetKind: "video"}) {
		t.Fatal("track compatibility mismatch")
	}
	for _, item := range []struct {
		track Track
		want  string
	}{
		{Track{TrackID: "visual_base"}, "visual"},
		{Track{TrackID: "sfx"}, "audio"},
		{Track{TrackID: "subtitles"}, "text"},
		{Track{TrackID: "custom_visual", TrackType: "video"}, "visual"},
		{Track{TrackID: "custom_audio", TrackType: "audio"}, "audio"},
		{Track{TrackID: "custom_text", TrackType: "text"}, "text"},
		{Track{TrackID: "custom", TrackType: "effects"}, "effects"},
	} {
		if got := trackFamily(item.track); got != item.want {
			t.Fatalf("track=%#v got=%q want=%q", item.track, got, item.want)
		}
	}

	for _, item := range []struct {
		value any
		want  float64
		ok    bool
	}{
		{float64(1.5), 1.5, true},
		{math.NaN(), math.NaN(), false},
		{math.Inf(1), math.Inf(1), false},
		{float32(2.5), 2.5, true},
		{float32(math.Inf(1)), math.Inf(1), false},
		{int(3), 3, true},
		{int64(4), 4, true},
		{"5", 0, false},
	} {
		got, ok := numericValue(item.value)
		if ok != item.ok || item.ok && got != item.want {
			t.Fatalf("numericValue(%#v)=(%v,%v)", item.value, got, ok)
		}
	}
	if effectiveRate(Clip{}) != 1 || effectiveRate(Clip{PlaybackRate: 2}) != 2 {
		t.Fatal("effective rate mismatch")
	}

	unsorted := Track{Clips: []Clip{
		{TimelineClipID: "z", TimelineStartFrame: 10},
		{TimelineClipID: "b", TimelineStartFrame: 0},
		{TimelineClipID: "a", TimelineStartFrame: 0},
	}}
	sortTrack(&unsorted)
	if unsorted.Clips[0].TimelineClipID != "a" || unsorted.Clips[1].TimelineClipID != "b" || unsorted.Clips[2].TimelineClipID != "z" {
		t.Fatalf("sorted=%#v", unsorted.Clips)
	}
	if clampInt(-1, 0, 10) != 0 || clampInt(11, 0, 10) != 10 || clampInt(5, 0, 10) != 5 {
		t.Fatal("clamp mismatch")
	}
}

func TestSemanticAnchorMetadataFollowsMoveRippleAndTrim(t *testing.T) {
	document := newEditingDocument(t, false)
	document.Tracks[1].Clips = []Clip{{
		TimelineClipID: "semantic_broll", TrackID: "visual_overlay",
		AssetID: "broll", AssetKind: "video", Role: "b_roll",
		TimelineStartFrame: 10, TimelineEndFrame: 20,
		SourceStartFrame: 0, SourceEndFrame: 10, PlaybackRate: 1,
		Metadata: map[string]any{
			"kind":                           "b_roll_semantic_anchor",
			"anchor_timeline_start_frame":    8,
			"anchor_timeline_end_frame":      24,
			"placement_timeline_start_frame": 10,
			"placement_timeline_end_frame":   20,
		},
	}}
	if err := moveClip(&document, map[string]any{
		"timeline_clip_id": "semantic_broll", "target_frame": 40, "mode": "overwrite",
	}); err != nil {
		t.Fatal(err)
	}
	clip := &document.Tracks[1].Clips[0]
	assertMetadataFrame := func(key string, want int) {
		t.Helper()
		value, ok := numericValue(clip.Metadata[key])
		if !ok || int(value) != want {
			t.Fatalf("%s=%#v want=%d", key, clip.Metadata[key], want)
		}
	}
	if clip.TimelineStartFrame != 40 || clip.TimelineEndFrame != 50 {
		t.Fatalf("moved clip=%#v", clip)
	}
	assertMetadataFrame("anchor_timeline_start_frame", 38)
	assertMetadataFrame("anchor_timeline_end_frame", 54)
	assertMetadataFrame("placement_timeline_start_frame", 40)
	assertMetadataFrame("placement_timeline_end_frame", 50)

	shiftAfter(&document, 30, 5)
	clip = &document.Tracks[1].Clips[0]
	if clip.TimelineStartFrame != 45 || clip.TimelineEndFrame != 55 {
		t.Fatalf("ripple clip=%#v", clip)
	}
	assertMetadataFrame("anchor_timeline_start_frame", 43)
	assertMetadataFrame("anchor_timeline_end_frame", 59)
	assertMetadataFrame("placement_timeline_start_frame", 45)
	assertMetadataFrame("placement_timeline_end_frame", 55)

	if err := trimClip(&document, map[string]any{
		"timeline_clip_id": "semantic_broll", "source_start_frame": 0, "source_end_frame": 6,
	}); err != nil {
		t.Fatal(err)
	}
	clip = &document.Tracks[1].Clips[0]
	if clip.TimelineEndFrame != 51 {
		t.Fatalf("trimmed clip=%#v", clip)
	}
	assertMetadataFrame("placement_timeline_start_frame", 45)
	assertMetadataFrame("placement_timeline_end_frame", 51)
}
