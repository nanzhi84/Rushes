package timeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

const DefaultFPS = 30

var requiredTracks = []struct {
	ID   string
	Type string
}{
	{"visual_base", "primary_visual"}, {"visual_overlay", "visual_overlay"},
	{"original_audio", "audio"}, {"voiceover", "audio"}, {"bgm", "audio"},
	{"subtitles", "text"},
}

type Document struct {
	TimelineID     string  `json:"timeline_id"`
	DraftID        string  `json:"draft_id"`
	Version        int     `json:"version"`
	FPS            int     `json:"fps"`
	DurationFrames int     `json:"duration_frames"`
	Tracks         []Track `json:"tracks"`
}

type Track struct {
	TrackID   string `json:"track_id"`
	TrackType string `json:"track_type"`
	Clips     []Clip `json:"clips"`
}

type Clip struct {
	TimelineClipID string           `json:"timeline_clip_id"`
	TrackID        string           `json:"track_id"`
	AssetID        string           `json:"asset_id,omitempty"`
	ClipID         *string          `json:"clip_id,omitempty"`
	Role           string           `json:"role,omitempty"`
	Text           string           `json:"text,omitempty"`
	TimelineStart  int              `json:"timeline_start_frame"`
	TimelineEnd    int              `json:"timeline_end_frame"`
	SourceStart    int              `json:"source_start_frame,omitempty"`
	SourceEnd      int              `json:"source_end_frame,omitempty"`
	PlaybackRate   float64          `json:"playback_rate,omitempty"`
	GainDB         float64          `json:"gain_db,omitempty"`
	LockPolicy     string           `json:"lock_policy,omitempty"`
	ParentBlockID  string           `json:"parent_block_id,omitempty"`
	Effects        []map[string]any `json:"effects,omitempty"`
}

type Selection struct {
	AssetID     string
	SourceStart float64
	SourceEnd   float64
	Role        string
}

type ValidationIssue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ValidationReport struct {
	Valid  bool              `json:"valid"`
	Checks []string          `json:"checks"`
	Issues []ValidationIssue `json:"issues"`
}

func ComposeInitial(draftID string, version int, selections []Selection) (Document, error) {
	if draftID == "" || version < 1 || len(selections) == 0 {
		return Document{}, errors.New("compose_initial 参数无效")
	}
	document := Empty(draftID, version)
	primary := &document.Tracks[0]
	cursor := 0
	for index, selection := range selections {
		if selection.AssetID == "" || selection.SourceStart < 0 || selection.SourceEnd <= selection.SourceStart {
			return Document{}, fmt.Errorf("clip %d 源范围无效", index)
		}
		duration := max(1, int(math.Round((selection.SourceEnd-selection.SourceStart)*float64(document.FPS))))
		clipID := fmt.Sprintf("clip_v%d_%03d", version, index+1)
		primary.Clips = append(primary.Clips, Clip{
			TimelineClipID: clipID, TrackID: primary.TrackID, AssetID: selection.AssetID,
			Role: selection.Role, TimelineStart: cursor, TimelineEnd: cursor + duration,
			SourceStart:  int(math.Round(selection.SourceStart * float64(document.FPS))),
			SourceEnd:    int(math.Round(selection.SourceEnd * float64(document.FPS))),
			PlaybackRate: 1, LockPolicy: "free", ParentBlockID: fmt.Sprintf("block_%03d", index+1),
		})
		cursor += duration
	}
	document.DurationFrames = cursor
	return document, nil
}

func Empty(draftID string, version int) Document {
	document := Document{
		TimelineID: fmt.Sprintf("%s:v%d", draftID, version), DraftID: draftID,
		Version: version, FPS: DefaultFPS, DurationFrames: 1,
	}
	for _, required := range requiredTracks {
		document.Tracks = append(document.Tracks, Track{
			TrackID: required.ID, TrackType: required.Type, Clips: []Clip{},
		})
	}
	return document
}

func Validate(document Document) ValidationReport {
	report := ValidationReport{Valid: true, Checks: []string{
		"schema", "required_tracks", "clip_ranges", "primary_visual_coverage",
	}, Issues: []ValidationIssue{}}
	add := func(code, message string) {
		report.Valid = false
		report.Issues = append(report.Issues, ValidationIssue{Code: code, Message: message})
	}
	if document.FPS <= 0 || document.DurationFrames <= 0 || document.Version < 1 {
		add("invalid_document", "fps、duration_frames、version 必须为正数")
	}
	tracks := map[string]Track{}
	for _, track := range document.Tracks {
		if _, duplicate := tracks[track.TrackID]; duplicate {
			add("duplicate_track", track.TrackID)
		}
		tracks[track.TrackID] = track
		for _, clip := range track.Clips {
			if clip.TimelineStart < 0 || clip.TimelineEnd <= clip.TimelineStart || clip.TimelineEnd > document.DurationFrames {
				add("invalid_clip_range", clip.TimelineClipID)
			}
			if clip.AssetID != "" && clip.SourceEnd <= clip.SourceStart {
				add("invalid_source_range", clip.TimelineClipID)
			}
			if clip.PlaybackRate < 0 {
				add("invalid_playback_rate", clip.TimelineClipID)
			}
		}
	}
	for _, required := range requiredTracks {
		if _, exists := tracks[required.ID]; !exists {
			add("missing_track", required.ID)
		}
	}
	primary, exists := tracks["visual_base"]
	if !exists || len(primary.Clips) == 0 {
		add("empty_primary_visual", "主视觉轨没有 clip")
	} else {
		sorted := append([]Clip(nil), primary.Clips...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].TimelineStart < sorted[j].TimelineStart })
		cursor := 0
		for _, clip := range sorted {
			if clip.TimelineStart != cursor {
				add("primary_visual_gap_or_overlap", clip.TimelineClipID)
			}
			cursor = clip.TimelineEnd
		}
		if cursor != document.DurationFrames {
			add("primary_visual_not_full_coverage", fmt.Sprintf("coverage=%d duration=%d", cursor, document.DurationFrames))
		}
	}
	return report
}

func ApplyPatch(document Document, operation map[string]any) (Document, error) {
	copy, err := clone(document)
	if err != nil {
		return Document{}, err
	}
	kind := stringValue(operation["kind"])
	switch kind {
	case "trim_clip":
		err = trimClip(&copy, operation)
	case "delete_range":
		err = deleteRange(&copy, operation)
	case "insert_clip":
		err = insertClip(&copy, operation)
	case "replace_clip":
		err = replaceClip(&copy, operation)
	case "set_playback_rate":
		err = setPlaybackRate(&copy, operation)
	case "adjust_gain":
		err = updateClip(&copy, operation, func(clip *Clip) error {
			clip.GainDB = numberValue(operation["gain_db"])
			return nil
		})
	case "edit_subtitle_text":
		err = updateClip(&copy, operation, func(clip *Clip) error {
			clip.Text = stringValue(operation["text"])
			return nil
		})
	case "remove_track_clips":
		err = removeTrackClips(&copy, stringValue(operation["track_id"]))
	default:
		return Document{}, fmt.Errorf("不支持的 patch op: %s", kind)
	}
	if err != nil {
		return Document{}, err
	}
	copy.Version++
	copy.TimelineID = fmt.Sprintf("%s:v%d", copy.DraftID, copy.Version)
	return copy, nil
}

func Inspect(document Document) string {
	counts := map[string]int{}
	for _, track := range document.Tracks {
		counts[track.TrackID] = len(track.Clips)
	}
	return fmt.Sprintf(
		"时间线 v%d：%.2f 秒，%d fps；主视觉 %d 段，叠加 %d 段，字幕 %d 段。",
		document.Version, float64(document.DurationFrames)/float64(document.FPS), document.FPS,
		counts["visual_base"], counts["visual_overlay"], counts["subtitles"],
	)
}

func ToMap(document Document) (map[string]any, error) {
	data, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	err = json.Unmarshal(data, &result)
	return result, err
}

func trimClip(document *Document, operation map[string]any) error {
	return updateClip(document, operation, func(clip *Clip) error {
		start := numberValue(operation["source_start_s"])
		end := numberValue(operation["source_end_s"])
		if start < 0 || end <= start {
			return errors.New("trim_clip 源范围无效")
		}
		oldDuration := clip.TimelineEnd - clip.TimelineStart
		clip.SourceStart = int(math.Round(start * float64(document.FPS)))
		clip.SourceEnd = int(math.Round(end * float64(document.FPS)))
		rate := clip.PlaybackRate
		if rate <= 0 {
			rate = 1
		}
		newDuration := max(1, int(math.Round((end-start)*float64(document.FPS)/rate)))
		clip.TimelineEnd = clip.TimelineStart + newDuration
		shiftAfter(document, clip.TimelineStart+oldDuration, newDuration-oldDuration)
		return nil
	})
}

func deleteRange(document *Document, operation map[string]any) error {
	start := int(math.Round(numberValue(operation["start_s"]) * float64(document.FPS)))
	end := int(math.Round(numberValue(operation["end_s"]) * float64(document.FPS)))
	if start < 0 || end <= start || end > document.DurationFrames {
		return errors.New("delete_range 范围无效")
	}
	delta := end - start
	for trackIndex := range document.Tracks {
		clips := document.Tracks[trackIndex].Clips
		kept := clips[:0]
		for _, clip := range clips {
			if clip.TimelineEnd <= start {
				kept = append(kept, clip)
				continue
			}
			if clip.TimelineStart >= end {
				clip.TimelineStart -= delta
				clip.TimelineEnd -= delta
				kept = append(kept, clip)
				continue
			}
			if clip.TimelineStart < start && clip.TimelineEnd > end {
				clip.TimelineEnd -= delta
				clip.SourceEnd -= delta
				kept = append(kept, clip)
			} else if clip.TimelineStart < start {
				clip.TimelineEnd = start
				clip.SourceEnd = clip.SourceStart + (clip.TimelineEnd - clip.TimelineStart)
				kept = append(kept, clip)
			} else if clip.TimelineEnd > end {
				removed := end - clip.TimelineStart
				clip.TimelineStart = start
				clip.TimelineEnd -= delta
				clip.SourceStart += removed
				kept = append(kept, clip)
			}
		}
		document.Tracks[trackIndex].Clips = kept
	}
	document.DurationFrames -= delta
	return nil
}

func insertClip(document *Document, operation map[string]any) error {
	assetID := stringValue(operation["asset_id"])
	start := numberValue(operation["source_start_s"])
	end := numberValue(operation["source_end_s"])
	if assetID == "" || start < 0 || end <= start {
		return errors.New("insert_clip 参数无效")
	}
	track := trackByID(document, valueOr(stringValue(operation["track_id"]), "visual_base"))
	if track == nil {
		return errors.New("insert_clip track 不存在")
	}
	duration := max(1, int(math.Round((end-start)*float64(document.FPS))))
	startFrame := document.DurationFrames
	clip := Clip{
		TimelineClipID: valueOr(stringValue(operation["timeline_clip_id"]), fmt.Sprintf("clip_v%d_%03d", document.Version+1, len(track.Clips)+1)),
		TrackID:        track.TrackID, AssetID: assetID, Role: valueOr(stringValue(operation["role"]), "b_roll"),
		TimelineStart: startFrame, TimelineEnd: startFrame + duration,
		SourceStart: int(math.Round(start * float64(document.FPS))), SourceEnd: int(math.Round(end * float64(document.FPS))),
		PlaybackRate: 1, LockPolicy: "free",
	}
	track.Clips = append(track.Clips, clip)
	if track.TrackID == "visual_base" {
		document.DurationFrames += duration
	}
	return nil
}

func replaceClip(document *Document, operation map[string]any) error {
	return updateClip(document, operation, func(clip *Clip) error {
		assetID := stringValue(operation["asset_id"])
		if assetID == "" {
			return errors.New("replace_clip 缺少 asset_id")
		}
		clip.AssetID = assetID
		if role := stringValue(operation["role"]); role != "" {
			clip.Role = role
		}
		return nil
	})
}

func setPlaybackRate(document *Document, operation map[string]any) error {
	return updateClip(document, operation, func(clip *Clip) error {
		rate := numberValue(operation["playback_rate"])
		if rate <= 0 || rate > 8 {
			return errors.New("playback_rate 必须在 (0,8]")
		}
		oldDuration := clip.TimelineEnd - clip.TimelineStart
		newDuration := max(1, int(math.Round(float64(clip.SourceEnd-clip.SourceStart)/rate)))
		clip.PlaybackRate = rate
		clip.TimelineEnd = clip.TimelineStart + newDuration
		shiftAfter(document, clip.TimelineStart+oldDuration, newDuration-oldDuration)
		return nil
	})
}

func updateClip(document *Document, operation map[string]any, update func(*Clip) error) error {
	id := valueOr(stringValue(operation["timeline_clip_id"]), stringValue(operation["clip_id"]))
	if id == "" {
		return errors.New("patch op 缺少 timeline_clip_id")
	}
	for trackIndex := range document.Tracks {
		for clipIndex := range document.Tracks[trackIndex].Clips {
			clip := &document.Tracks[trackIndex].Clips[clipIndex]
			if clip.TimelineClipID == id {
				return update(clip)
			}
		}
	}
	return fmt.Errorf("clip 不存在: %s", id)
}

func removeTrackClips(document *Document, trackID string) error {
	track := trackByID(document, trackID)
	if track == nil || trackID == "visual_base" {
		return errors.New("不能清空不存在的轨道或主视觉轨")
	}
	track.Clips = []Clip{}
	return nil
}

func shiftAfter(document *Document, boundary, delta int) {
	if delta == 0 {
		return
	}
	for trackIndex := range document.Tracks {
		for clipIndex := range document.Tracks[trackIndex].Clips {
			clip := &document.Tracks[trackIndex].Clips[clipIndex]
			if clip.TimelineStart >= boundary {
				clip.TimelineStart += delta
				clip.TimelineEnd += delta
			}
		}
	}
	document.DurationFrames += delta
}

func trackByID(document *Document, trackID string) *Track {
	for index := range document.Tracks {
		if document.Tracks[index].TrackID == trackID {
			return &document.Tracks[index]
		}
	}
	return nil
}

func clone(document Document) (Document, error) {
	data, err := json.Marshal(document)
	if err != nil {
		return Document{}, err
	}
	var result Document
	err = json.Unmarshal(data, &result)
	return result, err
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func numberValue(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	default:
		return 0
	}
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
