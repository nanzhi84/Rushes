package timeline

import "testing"

func redundantSplitFixture() Document {
	document := Empty("draft_coalesce", 7)
	document.DurationFrames = 60
	document.Tracks[0].Clips = []Clip{
		{
			TimelineClipID: "clip_left", TrackID: "visual_base",
			AssetID: "asset_video", AssetKind: "video", Role: "b_roll",
			TimelineStartFrame: 0, TimelineEndFrame: 30,
			SourceStartFrame: 100, SourceEndFrame: 130, PlaybackRate: 1,
			FadeInFrames: 4,
		},
		{
			TimelineClipID: "clip_right", TrackID: "visual_base",
			AssetID: "asset_video", AssetKind: "video", Role: "b_roll",
			TimelineStartFrame: 30, TimelineEndFrame: 60,
			SourceStartFrame: 130, SourceEndFrame: 160, PlaybackRate: 1,
			FadeOutFrames: 5,
		},
	}
	return document
}

func TestCoalesceRedundantAdjacentClipsPreservesOuterRenderingParameters(t *testing.T) {
	document := redundantSplitFixture()
	result, merges, err := CoalesceRedundantAdjacentClips(document)
	if err != nil {
		t.Fatal(err)
	}
	if len(merges) != 1 || merges[0].KeptTimelineClipID != "clip_left" ||
		merges[0].RemovedTimelineClipID != "clip_right" || merges[0].BoundaryFrame != 30 {
		t.Fatalf("merges=%#v", merges)
	}
	if len(result.Tracks[0].Clips) != 1 {
		t.Fatalf("clips=%#v", result.Tracks[0].Clips)
	}
	clip := result.Tracks[0].Clips[0]
	if clip.TimelineClipID != "clip_left" || clip.TimelineStartFrame != 0 ||
		clip.TimelineEndFrame != 60 || clip.SourceStartFrame != 100 ||
		clip.SourceEndFrame != 160 || clip.FadeInFrames != 4 || clip.FadeOutFrames != 5 {
		t.Fatalf("merged clip=%#v", clip)
	}
	if document.Tracks[0].Clips[0].TimelineEndFrame != 30 ||
		len(document.Tracks[0].Clips) != 2 || result.Version != document.Version {
		t.Fatalf("input or version mutated: input=%#v result_version=%d", document.Tracks[0].Clips, result.Version)
	}
	if report := Validate(result); !report.Valid {
		t.Fatalf("validation=%#v", report)
	}
}

func TestCoalesceRedundantAdjacentClipsKeepsIntentionalBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Document)
	}{
		{"source discontinuity", func(document *Document) {
			document.Tracks[0].Clips[1].SourceStartFrame++
		}},
		{"internal fade", func(document *Document) {
			document.Tracks[0].Clips[0].FadeOutFrames = 2
		}},
		{"effect difference", func(document *Document) {
			document.Tracks[0].Clips[1].Effects = []map[string]any{{"kind": "zoom"}}
		}},
		{"playback rate difference", func(document *Document) {
			document.Tracks[0].Clips[1].PlaybackRate = 0.5
		}},
		{"linked media", func(document *Document) {
			document.Tracks[0].Clips[0].Linked = true
			document.Tracks[0].Clips[0].ParentBlockID = "group_left"
		}},
		{"locked track", func(document *Document) {
			document.Tracks[0].Locked = true
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := redundantSplitFixture()
			test.mutate(&document)
			result, merges, err := CoalesceRedundantAdjacentClips(document)
			if err != nil {
				t.Fatal(err)
			}
			if len(merges) != 0 || len(result.Tracks[0].Clips) != 2 {
				t.Fatalf("merges=%#v clips=%#v", merges, result.Tracks[0].Clips)
			}
		})
	}
}

func TestCoalesceRedundantAdjacentClipsMergesSplitSemanticPlacementMetadata(t *testing.T) {
	document := redundantSplitFixture()
	left := &document.Tracks[0].Clips[0]
	right := &document.Tracks[0].Clips[1]
	left.Metadata = map[string]any{
		"kind": "b_roll_semantic_anchor", "anchor_timeline_start_frame": 0,
		"anchor_timeline_end_frame": 60, "placement_timeline_start_frame": 0,
		"placement_timeline_end_frame": 30,
	}
	right.Metadata = map[string]any{
		"kind": "b_roll_semantic_anchor", "anchor_timeline_start_frame": 0,
		"anchor_timeline_end_frame": 60, "placement_timeline_start_frame": 30,
		"placement_timeline_end_frame": 60,
	}
	result, merges, err := CoalesceRedundantAdjacentClips(document)
	if err != nil || len(merges) != 1 {
		t.Fatalf("merges=%#v err=%v", merges, err)
	}
	metadata := result.Tracks[0].Clips[0].Metadata
	if metadata["placement_timeline_start_frame"] != 0 ||
		metadata["placement_timeline_end_frame"] != 60 {
		t.Fatalf("metadata=%#v", metadata)
	}
}

func TestCoalesceApprovedOverlappingAdjacentClipsLinearizesRepeatedSourceFrames(t *testing.T) {
	document := redundantSplitFixture()
	document.Tracks[0].Clips[1].SourceStartFrame = 120
	document.Tracks[0].Clips[1].SourceEndFrame = 150
	result, merges, err := CoalesceApprovedOverlappingAdjacentClips(
		document,
		[]AdjacentClipOverlapRepair{{
			TrackID: "visual_base", KeptTimelineClipID: "clip_left",
			RemovedTimelineClipID: "clip_right", NormalizedSourceEndFrame: 160,
			ShotID: "shot_wet_sand",
		}},
	)
	if err != nil || len(merges) != 1 {
		t.Fatalf("merges=%#v err=%v", merges, err)
	}
	if len(result.Tracks[0].Clips) != 1 {
		t.Fatalf("clips=%#v", result.Tracks[0].Clips)
	}
	clip := result.Tracks[0].Clips[0]
	if clip.SourceStartFrame != 100 || clip.SourceEndFrame != 160 ||
		clip.TimelineStartFrame != 0 || clip.TimelineEndFrame != 60 {
		t.Fatalf("clip=%#v", clip)
	}
	merge := merges[0]
	if merge.Mode != "same_shot_overlap_repair" || merge.SourceOverlapFrames != 10 ||
		merge.PreviousSourceEndFrame != 150 || merge.NormalizedSourceEndFrame != 160 ||
		merge.ShotID != "shot_wet_sand" {
		t.Fatalf("merge=%#v", merge)
	}
	if report := Validate(result); !report.Valid {
		t.Fatalf("validation=%#v", report)
	}
}

func TestCoalesceApprovedOverlappingAdjacentClipsRejectsUnprovenRewrite(t *testing.T) {
	document := redundantSplitFixture()
	document.Tracks[0].Clips[1].SourceStartFrame = 120
	document.Tracks[0].Clips[1].SourceEndFrame = 150
	_, _, err := CoalesceApprovedOverlappingAdjacentClips(
		document,
		[]AdjacentClipOverlapRepair{{
			TrackID: "visual_base", KeptTimelineClipID: "clip_left",
			RemovedTimelineClipID: "clip_right", NormalizedSourceEndFrame: 150,
		}},
	)
	if err == nil {
		t.Fatal("repair without the exact duration-preserving source end must fail")
	}
}

func TestCoalesceApprovedOverlappingAdjacentClipsRejectsInvalidApprovals(t *testing.T) {
	document := redundantSplitFixture()
	document.Tracks[0].Clips[1].SourceStartFrame = 120
	document.Tracks[0].Clips[1].SourceEndFrame = 150
	valid := AdjacentClipOverlapRepair{
		TrackID: "visual_base", KeptTimelineClipID: "clip_left",
		RemovedTimelineClipID: "clip_right", NormalizedSourceEndFrame: 160,
	}
	tests := []struct {
		name    string
		repairs []AdjacentClipOverlapRepair
	}{
		{"empty clip id", []AdjacentClipOverlapRepair{{TrackID: "visual_base"}}},
		{"duplicate approval", []AdjacentClipOverlapRepair{valid, valid}},
		{"wrong track", []AdjacentClipOverlapRepair{{
			TrackID: "visual_overlay", KeptTimelineClipID: "clip_left",
			RemovedTimelineClipID: "clip_right", NormalizedSourceEndFrame: 160,
		}}},
		{"missing boundary", []AdjacentClipOverlapRepair{{
			TrackID: "visual_base", KeptTimelineClipID: "missing_left",
			RemovedTimelineClipID: "missing_right", NormalizedSourceEndFrame: 160,
		}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := CoalesceApprovedOverlappingAdjacentClips(document, test.repairs); err == nil {
				t.Fatal("invalid Harness approval must fail closed")
			}
		})
	}
}

func TestCoalesceApprovedOverlappingAdjacentClipsNoRepairsReturnsClone(t *testing.T) {
	document := redundantSplitFixture()
	result, merges, err := CoalesceApprovedOverlappingAdjacentClips(document, nil)
	if err != nil || len(merges) != 0 || len(result.Tracks[0].Clips) != 2 {
		t.Fatalf("result=%#v merges=%#v err=%v", result, merges, err)
	}
	result.Tracks[0].Clips[0].TimelineEndFrame = 12
	if document.Tracks[0].Clips[0].TimelineEndFrame != 30 {
		t.Fatal("no-op normalization must still return an independent clone")
	}
}

func TestCoalesceFunctionsRejectUnserializableDocuments(t *testing.T) {
	document := redundantSplitFixture()
	document.Tracks[0].Clips[0].Metadata = map[string]any{"invalid": func() {}}
	if _, _, err := CoalesceRedundantAdjacentClips(document); err == nil {
		t.Fatal("redundant split normalization must surface clone failures")
	}
	if _, _, err := CoalesceApprovedOverlappingAdjacentClips(document, nil); err == nil {
		t.Fatal("approved overlap normalization must surface clone failures")
	}
}

func TestCoalesceRedundantAdjacentClipsKeepsDifferentSemanticMetadata(t *testing.T) {
	document := redundantSplitFixture()
	document.Tracks[0].Clips[0].Metadata = map[string]any{"kind": "b_roll_semantic_anchor"}
	document.Tracks[0].Clips[1].Metadata = map[string]any{"kind": "other"}
	result, merges, err := CoalesceRedundantAdjacentClips(document)
	if err != nil || len(merges) != 0 || len(result.Tracks[0].Clips) != 2 {
		t.Fatalf("result=%#v merges=%#v err=%v", result, merges, err)
	}
}
