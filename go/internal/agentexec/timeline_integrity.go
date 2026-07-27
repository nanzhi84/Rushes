package agentexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sort"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

var independentAudioTrackIDs = []string{"bgm", "sfx"}

const independentAudioPreservationProofType = "bounded_independent_audio_v1"

type independentAudioPreservationProof struct {
	Type           string `json:"type"`
	TrackID        string `json:"track_id"`
	TimingSHA256   string `json:"timing_sha256"`
	OverhangFrames int    `json:"overhang_frames"`
}

type independentAudioTrackTiming struct {
	TrackID string                       `json:"track_id"`
	Clips   []independentAudioClipTiming `json:"clips"`
}

type independentAudioClipTiming struct {
	TimelineClipID     string  `json:"timeline_clip_id"`
	TrackID            string  `json:"track_id"`
	AssetID            string  `json:"asset_id"`
	AssetKind          string  `json:"asset_kind"`
	Role               string  `json:"role"`
	TimelineStartFrame int     `json:"timeline_start_frame"`
	TimelineEndFrame   int     `json:"timeline_end_frame"`
	SourceStartFrame   int     `json:"source_start_frame"`
	SourceEndFrame     int     `json:"source_end_frame"`
	PlaybackRate       float64 `json:"playback_rate"`
	ParentBlockID      string  `json:"parent_block_id"`
	Linked             bool    `json:"linked"`
}

func preserveIndependentAudioForOperation(
	current timeline.Document,
	operation map[string]any,
) map[string]timeline.Track {
	touched := touchedTrackIDsForOperation(current, operation)
	preserved := map[string]timeline.Track{}
	for _, track := range current.Tracks {
		if !ContainsString(independentAudioTrackIDs, track.TrackID) {
			continue
		}
		if _, changed := touched[track.TrackID]; changed {
			continue
		}
		preserved[track.TrackID] = copyTimelineTrack(track)
	}
	return preserved
}

func touchedTrackIDsForOperation(
	current timeline.Document,
	operation map[string]any,
) map[string]struct{} {
	clipTracks := map[string]string{}
	for _, track := range current.Tracks {
		for _, clip := range track.Clips {
			clipTracks[clip.TimelineClipID] = track.TrackID
		}
	}
	touched := map[string]struct{}{}
	kind := StringValue(operation["kind"])
	if kind == "delete_range" || kind == "delete_source_range" {
		for _, trackID := range independentAudioTrackIDs {
			touched[trackID] = struct{}{}
		}
	}
	trackID := StringValue(operation["track_id"])
	if kind == "insert_clip" && trackID == "" {
		trackID = "visual_base"
	}
	if trackID != "" {
		touched[trackID] = struct{}{}
	}
	if targetTrackID := StringValue(operation["target_track_id"]); targetTrackID != "" {
		touched[targetTrackID] = struct{}{}
	}
	clipID := StringValue(operation["timeline_clip_id"])
	if sourceTrackID := clipTracks[clipID]; sourceTrackID != "" {
		touched[sourceTrackID] = struct{}{}
	}
	return touched
}

func restoreIndependentAudioTracks(
	document *timeline.Document,
	preserved map[string]timeline.Track,
) {
	for trackIndex := range document.Tracks {
		track, exists := preserved[document.Tracks[trackIndex].TrackID]
		if !exists {
			continue
		}
		document.Tracks[trackIndex] = copyTimelineTrack(track)
	}
}

func unlockPreservedIndependentAudio(
	document timeline.Document,
	preserved map[string]timeline.Track,
) timeline.Document {
	copy := document
	copy.Tracks = append([]timeline.Track(nil), document.Tracks...)
	for trackIndex := range copy.Tracks {
		if _, exists := preserved[copy.Tracks[trackIndex].TrackID]; exists {
			copy.Tracks[trackIndex].Locked = false
		}
	}
	return copy
}

func validateWithPreservedIndependentAudio(
	document timeline.Document,
	preserved map[string]timeline.Track,
) timeline.ValidationReport {
	report := timeline.Validate(document)
	if report.Valid || len(preserved) == 0 {
		return report
	}
	preservedClipIDs := map[string]struct{}{}
	for _, track := range document.Tracks {
		snapshot, exists := preserved[track.TrackID]
		if !exists || !ContainsString(independentAudioTrackIDs, track.TrackID) ||
			!reflect.DeepEqual(track, snapshot) {
			continue
		}
		for _, clip := range track.Clips {
			if clip.TrackID == track.TrackID &&
				clip.TimelineStartFrame >= 0 &&
				clip.TimelineEndFrame > clip.TimelineStartFrame &&
				clip.TimelineEndFrame > document.DurationFrames {
				preservedClipIDs[clip.TimelineClipID] = struct{}{}
			}
		}
	}
	issues := report.Issues[:0]
	for _, issue := range report.Issues {
		if issue.Code == "invalid_clip_range" {
			if _, allowed := preservedClipIDs[issue.Message]; allowed {
				continue
			}
		}
		issues = append(issues, issue)
	}
	report.Issues = issues
	report.Valid = len(issues) == 0
	return report
}

func deriveIndependentAudioValidationProof(
	current timeline.Document,
	result timeline.Document,
	trustedCurrent map[string]timeline.Track,
	currentValid bool,
) map[string]timeline.Track {
	allowed := map[string]timeline.Track{}
	if !currentValid {
		return allowed
	}
	currentTracks := make(map[string]timeline.Track, len(current.Tracks))
	for _, track := range current.Tracks {
		currentTracks[track.TrackID] = track
	}
	for _, track := range result.Tracks {
		if !ContainsString(independentAudioTrackIDs, track.TrackID) ||
			trackOverhangFrames(track, result.DurationFrames) == 0 {
			continue
		}
		previous, exists := currentTracks[track.TrackID]
		if !exists {
			continue
		}
		if reflect.DeepEqual(independentAudioTiming(previous), independentAudioTiming(track)) {
			allowed[track.TrackID] = copyTimelineTrack(track)
			continue
		}
		if _, trusted := trustedCurrent[track.TrackID]; trusted &&
			trackOverhangFrames(track, result.DurationFrames) <=
				trackOverhangFrames(previous, current.DurationFrames) {
			allowed[track.TrackID] = copyTimelineTrack(track)
		}
	}
	return allowed
}

func (exec *Executor) validateStoredTimeline(
	ctx context.Context,
	draftID string,
	document timeline.Document,
) (timeline.ValidationReport, error) {
	preserved, err := exec.preservedIndependentAudioFromStoredProof(ctx, draftID, document)
	if err != nil {
		return timeline.ValidationReport{}, err
	}
	return validateWithPreservedIndependentAudio(document, preserved), nil
}

func (exec *Executor) preservedIndependentAudioFromStoredProof(
	ctx context.Context,
	draftID string,
	document timeline.Document,
) (map[string]timeline.Track, error) {
	version := document.Version
	logicalSnapshot := document
	visited := map[int]struct{}{}
	for depth := 0; depth < 64; depth++ {
		if _, duplicate := visited[version]; duplicate {
			return nil, errors.New("timeline preservation proof parent chain contains a cycle")
		}
		visited[version] = struct{}{}
		var parentVersion sql.NullInt64
		var rawReport sql.NullString
		err := exec.database.Read().QueryRowContext(ctx, `
			SELECT parent_version,validation_report_json
			FROM timeline_versions WHERE draft_id=? AND version=?`,
			draftID, version,
		).Scan(&parentVersion, &rawReport)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if rawReport.Valid {
			proofs, authoritative, proofErr := verifiedIndependentAudioProofs(document, rawReport.String)
			if proofErr != nil {
				return nil, proofErr
			}
			if authoritative {
				return proofs, nil
			}
		}
		if !parentVersion.Valid {
			return nil, nil
		}
		parent, parentErr := timeline.Get(ctx, exec.database, draftID, int(parentVersion.Int64))
		if errors.Is(parentErr, storage.ErrNotFound) {
			return nil, nil
		}
		if parentErr != nil {
			return nil, parentErr
		}
		if !sameLogicalTimelineSnapshot(logicalSnapshot, parent) {
			return nil, nil
		}
		logicalSnapshot = parent
		version = int(parentVersion.Int64)
	}
	return nil, errors.New("timeline preservation proof parent chain exceeds 64 versions")
}

func addIndependentAudioPreservationProofs(
	report map[string]any,
	document timeline.Document,
	preserved map[string]timeline.Track,
) error {
	proofs := []independentAudioPreservationProof{}
	for _, trackID := range independentAudioTrackIDs {
		track, exists := preserved[trackID]
		if !exists || !trackHasOverhang(track, document.DurationFrames) {
			continue
		}
		hash, err := timelineTrackTimingSHA256(track)
		if err != nil {
			return err
		}
		proofs = append(proofs, independentAudioPreservationProof{
			Type: independentAudioPreservationProofType, TrackID: trackID,
			TimingSHA256: hash, OverhangFrames: trackOverhangFrames(track, document.DurationFrames),
		})
	}
	if len(proofs) > 0 {
		report["preservation_proofs"] = proofs
	}
	return nil
}

func verifiedIndependentAudioProofs(
	document timeline.Document,
	rawReport string,
) (map[string]timeline.Track, bool, error) {
	var stored struct {
		Proofs json.RawMessage `json:"preservation_proofs"`
	}
	if err := json.Unmarshal([]byte(rawReport), &stored); err != nil {
		return nil, false, err
	}
	if len(stored.Proofs) == 0 {
		return nil, false, nil
	}
	var proofs []independentAudioPreservationProof
	if err := json.Unmarshal(stored.Proofs, &proofs); err != nil {
		return nil, true, err
	}
	tracks := make(map[string]timeline.Track, len(document.Tracks))
	for _, track := range document.Tracks {
		tracks[track.TrackID] = track
	}
	preserved := map[string]timeline.Track{}
	for _, proof := range proofs {
		track, exists := tracks[proof.TrackID]
		if proof.Type != independentAudioPreservationProofType ||
			!ContainsString(independentAudioTrackIDs, proof.TrackID) || !exists {
			continue
		}
		hash, err := timelineTrackTimingSHA256(track)
		if err != nil {
			return nil, true, err
		}
		if hash == proof.TimingSHA256 && proof.OverhangFrames > 0 &&
			trackOverhangFrames(track, document.DurationFrames) == proof.OverhangFrames {
			preserved[proof.TrackID] = copyTimelineTrack(track)
		}
	}
	return preserved, true, nil
}

func timelineTrackTimingSHA256(track timeline.Track) (string, error) {
	raw, err := json.Marshal(independentAudioTiming(track))
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}

func independentAudioTiming(track timeline.Track) independentAudioTrackTiming {
	timing := independentAudioTrackTiming{TrackID: track.TrackID}
	for _, clip := range track.Clips {
		timing.Clips = append(timing.Clips, independentAudioClipTiming{
			TimelineClipID: clip.TimelineClipID, TrackID: clip.TrackID,
			AssetID: clip.AssetID, AssetKind: clip.AssetKind, Role: clip.Role,
			TimelineStartFrame: clip.TimelineStartFrame, TimelineEndFrame: clip.TimelineEndFrame,
			SourceStartFrame: clip.SourceStartFrame, SourceEndFrame: clip.SourceEndFrame,
			PlaybackRate: clip.PlaybackRate, ParentBlockID: clip.ParentBlockID, Linked: clip.Linked,
		})
	}
	return timing
}

func trackHasOverhang(track timeline.Track, durationFrames int) bool {
	return trackOverhangFrames(track, durationFrames) > 0
}

func trackOverhangFrames(track timeline.Track, durationFrames int) int {
	overhang := 0
	for _, clip := range track.Clips {
		overhang = max(overhang, clip.TimelineEndFrame-durationFrames)
	}
	return overhang
}

func sameLogicalTimelineSnapshot(left, right timeline.Document) bool {
	left.TimelineID, left.DraftID, left.Version = "", "", 0
	right.TimelineID, right.DraftID, right.Version = "", "", 0
	return reflect.DeepEqual(left, right)
}

func copyTimelineTrack(track timeline.Track) timeline.Track {
	copy := track
	copy.Clips = append([]timeline.Clip(nil), track.Clips...)
	return copy
}

func BeatAlignmentData(document timeline.Document) map[string]any {
	beatFrames := []int{}
	strongFrames := []int{}
	downbeatFrames := []int{}
	for _, track := range document.Tracks {
		if track.TrackID != "bgm" {
			continue
		}
		for _, clip := range track.Clips {
			if explicitGrid, ok := clip.Metadata["beat_grid"].(map[string]any); ok {
				beatFrames = append(beatFrames, mapEffectFramesToTimeline(clip, explicitGrid["beat_frames"])...)
				strongFrames = append(strongFrames, mapEffectFramesToTimeline(clip, explicitGrid["strong_beat_frames"])...)
				downbeatFrames = append(downbeatFrames, mapEffectFramesToTimeline(clip, explicitGrid["downbeat_frames"])...)
			}
			for _, effect := range clip.Effects {
				if StringValue(effect["kind"]) != "beat_grid" {
					continue
				}
				beatFrames = append(beatFrames, mapEffectFramesToTimeline(clip, effect["beat_frames"])...)
				strongFrames = append(strongFrames, mapEffectFramesToTimeline(clip, effect["strong_beat_frames"])...)
				downbeatFrames = append(downbeatFrames, mapEffectFramesToTimeline(clip, effect["downbeat_frames"])...)
			}
		}
	}
	beatFrames = sortedUniqueInts(beatFrames)
	strongFrames = sortedUniqueInts(strongFrames)
	downbeatFrames = sortedUniqueInts(downbeatFrames)
	cutFrames := []int{}
	for _, track := range document.Tracks {
		if track.TrackID != "visual_base" {
			continue
		}
		clips := append([]timeline.Clip(nil), track.Clips...)
		sort.SliceStable(clips, func(i, j int) bool {
			return clips[i].TimelineStartFrame < clips[j].TimelineStartFrame
		})
		for index, clip := range clips {
			if clip.TimelineEndFrame > 0 && clip.TimelineEndFrame < document.DurationFrames {
				if index+1 < len(clips) && clipsHaveContinuousSourceBoundary(clip, clips[index+1]) {
					continue
				}
				cutFrames = append(cutFrames, clip.TimelineEndFrame)
			}
		}
	}
	onBeat := 0
	onAccent := 0
	offBeat := []int{}
	for _, frame := range cutFrames {
		if ContainsFrame(beatFrames, frame) {
			onBeat++
		} else {
			offBeat = append(offBeat, frame)
		}
		if ContainsFrame(strongFrames, frame) || ContainsFrame(downbeatFrames, frame) {
			onAccent++
		}
	}
	ratio := 0.0
	if len(cutFrames) > 0 {
		ratio = math.Round(float64(onBeat)/float64(len(cutFrames))*1000) / 1000
	}
	result := map[string]any{
		"beat_grid_present":     len(beatFrames) > 0,
		"cut_count":             len(cutFrames),
		"on_beat_cut_count":     onBeat,
		"on_accent_cut_count":   onAccent,
		"alignment_ratio":       ratio,
		"off_beat_cut_frames":   offBeat,
		"all_cuts_on_beat_grid": len(cutFrames) > 0 && onBeat == len(cutFrames),
		"beat_marker_count":     len(beatFrames),
		"strong_marker_count":   len(strongFrames),
		"downbeat_marker_count": len(downbeatFrames),
	}
	if len(beatFrames) == 0 {
		result["warning"] = "BGM 缺少 beat_grid 元数据；结构校验不能证明画面切点已卡音乐节拍"
	}
	return result
}

func clipsHaveContinuousSourceBoundary(previous, next timeline.Clip) bool {
	return previous.AssetID != "" && previous.AssetID == next.AssetID &&
		previous.SourceEndFrame == next.SourceStartFrame
}

func mapEffectFramesToTimeline(clip timeline.Clip, value any) []int {
	rate := clip.PlaybackRate
	if rate <= 0 {
		rate = 1
	}
	frames := []int{}
	for _, sourceFrame := range EffectFrameValues(value) {
		if sourceFrame < clip.SourceStartFrame || sourceFrame > clip.SourceEndFrame {
			continue
		}
		frames = append(frames, clip.TimelineStartFrame+int(math.Round(
			float64(sourceFrame-clip.SourceStartFrame)/rate,
		)))
	}
	return frames
}

func EffectFrameValues(value any) []int {
	result := []int{}
	switch frames := value.(type) {
	case []int:
		result = append(result, frames...)
	case []float64:
		for _, frame := range frames {
			if !math.IsNaN(frame) && !math.IsInf(frame, 0) && frame >= 0 && frame == math.Trunc(frame) {
				result = append(result, int(frame))
			}
		}
	case []any:
		for _, raw := range frames {
			if frame, ok := NumericValue(raw); ok && frame >= 0 && frame == math.Trunc(frame) {
				result = append(result, int(frame))
			}
		}
	}
	return result
}

func sortedUniqueInts(values []int) []int {
	sort.Ints(values)
	result := values[:0]
	previous := -1
	for _, value := range values {
		if value == previous {
			continue
		}
		result = append(result, value)
		previous = value
	}
	return result
}

func ContainsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func StringValue(value any) string {
	text, _ := value.(string)
	return text
}

func ValueOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func (exec *Executor) enrichTimelineOperation(
	ctx context.Context,
	draftID string,
	original map[string]any,
) (map[string]any, error) {
	assets, err := storage.ListDraftAssets(ctx, exec.database.Read(), draftID)
	if err != nil {
		return nil, err
	}
	assetByID := make(map[string]storage.Asset, len(assets))
	for _, asset := range assets {
		assetByID[asset.ID] = asset
	}

	operation := make(map[string]any, len(original)+2)
	for key, value := range original {
		operation[key] = value
	}
	switch StringValue(operation["kind"]) {
	case "insert_clip", "replace_clip":
		asset, exists := assetByID[StringValue(operation["asset_id"])]
		if !exists {
			break
		}
		if StringValue(operation["asset_kind"]) == "" {
			operation["asset_kind"] = asset.Kind
		}
		if StringValue(operation["kind"]) == "replace_clip" {
			break
		}
		if ValueOr(StringValue(operation["track_id"]), "visual_base") != "visual_base" {
			break
		}
		if _, explicit := operation["include_original_audio"]; !explicit {
			hasAudio, _ := asset.Probe["has_audio"].(bool)
			operation["include_original_audio"] = asset.Kind == "video" && hasAudio
		}
	}
	return operation, nil
}
