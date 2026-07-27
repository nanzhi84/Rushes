package agentexec

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

var independentAudioTrackIDs = []string{"bgm", "sfx"}

const (
	independentAudioPreservationProofType = "bounded_independent_audio_v1"
	preservedAudioLineageMetadataKey      = "_rushes_preserved_audio_lineage"
)

type independentAudioPreservationProof struct {
	Type           string `json:"type"`
	TrackID        string `json:"track_id"`
	TimingSHA256   string `json:"timing_sha256"`
	OverhangFrames int    `json:"overhang_frames"`
}

type preservedAudioLineageContext struct {
	prefix string
}

func newPreservedAudioLineageContext() (preservedAudioLineageContext, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return preservedAudioLineageContext{}, fmt.Errorf("生成音频 lineage token: %w", err)
	}
	return preservedAudioLineageContext{
		prefix: "rushes-internal-audio-lineage:" + hex.EncodeToString(nonce[:]),
	}, nil
}

func (context preservedAudioLineageContext) markerValue(timelineClipID string) string {
	return context.prefix + ":" + timelineClipID
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
	touchedClips := touchedTimelineClipIDsForOperation(current, operation)
	preserved := map[string]timeline.Track{}
	for _, track := range current.Tracks {
		if !ContainsString(independentAudioTrackIDs, track.TrackID) {
			continue
		}
		_, trackChanged := touched[track.TrackID]
		if trackChanged && len(touchedClips) == 0 {
			continue
		}
		snapshot := copyTimelineTrack(track)
		snapshot.Clips = snapshot.Clips[:0]
		for _, clip := range track.Clips {
			if clip.Linked {
				continue
			}
			if _, changed := touchedClips[clip.TimelineClipID]; trackChanged && changed {
				continue
			}
			snapshot.Clips = append(snapshot.Clips, clip)
		}
		if len(snapshot.Clips) > 0 {
			preserved[track.TrackID] = snapshot
		}
	}
	return preserved
}

func touchedTimelineClipIDsForOperation(
	current timeline.Document,
	operation map[string]any,
) map[string]struct{} {
	touched := map[string]struct{}{}
	kind := StringValue(operation["kind"])
	if kind == "delete_range" || kind == "delete_source_range" {
		for _, track := range current.Tracks {
			for _, clip := range track.Clips {
				touched[clip.TimelineClipID] = struct{}{}
			}
		}
		return touched
	}
	targetID := StringValue(operation["timeline_clip_id"])
	if targetID == "" {
		return touched
	}
	linkedGroupID := ""
	sourceTrackID := ""
	for _, track := range current.Tracks {
		for _, clip := range track.Clips {
			if clip.TimelineClipID == targetID {
				touched[targetID] = struct{}{}
				sourceTrackID = track.TrackID
				if clip.Linked {
					linkedGroupID = clip.ParentBlockID
				}
			}
		}
	}
	if linkedGroupID != "" {
		for _, track := range current.Tracks {
			for _, clip := range track.Clips {
				if clip.Linked && clip.ParentBlockID == linkedGroupID {
					touched[clip.TimelineClipID] = struct{}{}
				}
			}
		}
	}
	if kind == "move_clip" {
		targetTrackID := StringValue(operation["target_track_id"])
		if targetTrackID == "" {
			targetTrackID = sourceTrackID
		}
		for _, track := range current.Tracks {
			if track.TrackID != targetTrackID {
				continue
			}
			for _, clip := range track.Clips {
				touched[clip.TimelineClipID] = struct{}{}
			}
		}
	}
	return touched
}

func touchedTrackIDsForOperation(
	current timeline.Document,
	operation map[string]any,
) map[string]struct{} {
	clipTracks := map[string]string{}
	clipLinkedGroups := map[string]string{}
	linkedGroupTracks := map[string]map[string]struct{}{}
	for _, track := range current.Tracks {
		for _, clip := range track.Clips {
			clipTracks[clip.TimelineClipID] = track.TrackID
			if !clip.Linked || clip.ParentBlockID == "" {
				continue
			}
			clipLinkedGroups[clip.TimelineClipID] = clip.ParentBlockID
			if linkedGroupTracks[clip.ParentBlockID] == nil {
				linkedGroupTracks[clip.ParentBlockID] = map[string]struct{}{}
			}
			linkedGroupTracks[clip.ParentBlockID][track.TrackID] = struct{}{}
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
	if linkedGroupID := clipLinkedGroups[clipID]; linkedGroupID != "" {
		for linkedTrackID := range linkedGroupTracks[linkedGroupID] {
			touched[linkedTrackID] = struct{}{}
		}
	}
	return touched
}

func restoreIndependentAudioTracks(
	document *timeline.Document,
	current timeline.Document,
	preserved map[string]timeline.Track,
	lineageContext preservedAudioLineageContext,
) error {
	protectedIDs := make(map[string]struct{})
	for _, track := range preserved {
		for _, clip := range track.Clips {
			protectedIDs[clip.TimelineClipID] = struct{}{}
		}
	}
	protectedCounts := make(map[string]int, len(protectedIDs))
	for _, track := range document.Tracks {
		for _, clip := range track.Clips {
			if _, protected := protectedIDs[clip.TimelineClipID]; protected {
				lineageID, marked := preservedAudioLineageID(clip, lineageContext)
				if !marked || lineageID != clip.TimelineClipID {
					return fmt.Errorf("受保护音频片段 ID 冲突: duplicate timeline_clip_id %q", clip.TimelineClipID)
				}
				protectedCounts[clip.TimelineClipID]++
			}
		}
	}
	for clipID, count := range protectedCounts {
		if count > 1 {
			return fmt.Errorf("受保护音频片段 ID 冲突: duplicate timeline_clip_id %q", clipID)
		}
	}

	currentTracks := make(map[string]timeline.Track, len(current.Tracks))
	for _, track := range current.Tracks {
		currentTracks[track.TrackID] = track
	}
	for trackIndex := range document.Tracks {
		snapshot, exists := preserved[document.Tracks[trackIndex].TrackID]
		if !exists {
			continue
		}
		currentTrack, found := currentTracks[document.Tracks[trackIndex].TrackID]
		if found && reflect.DeepEqual(currentTrack.Clips, snapshot.Clips) {
			document.Tracks[trackIndex] = copyTimelineTrack(snapshot)
			continue
		}
		preservedByID := make(map[string]timeline.Clip, len(snapshot.Clips))
		for _, clip := range snapshot.Clips {
			preservedByID[clip.TimelineClipID] = clip
		}
		restored := copyTimelineTrack(document.Tracks[trackIndex])
		kept := make([]timeline.Clip, 0, len(restored.Clips)+len(snapshot.Clips))
		for _, clip := range restored.Clips {
			if lineageID, marked := preservedAudioLineageID(clip, lineageContext); marked {
				if _, protected := preservedByID[lineageID]; protected {
					continue
				}
			}
			kept = append(kept, clip)
		}
		restored.Clips = append(kept, snapshot.Clips...)
		sort.SliceStable(restored.Clips, func(left, right int) bool {
			if restored.Clips[left].TimelineStartFrame == restored.Clips[right].TimelineStartFrame {
				return restored.Clips[left].TimelineClipID < restored.Clips[right].TimelineClipID
			}
			return restored.Clips[left].TimelineStartFrame < restored.Clips[right].TimelineStartFrame
		})
		restored.Muted = snapshot.Muted
		restored.Solo = snapshot.Solo
		restored.Locked = snapshot.Locked
		restored.GainDB = snapshot.GainDB
		restored.Ducking = snapshot.Ducking
		document.Tracks[trackIndex] = restored
	}
	return nil
}

func preservedAudioLineageID(
	clip timeline.Clip,
	lineageContext preservedAudioLineageContext,
) (string, bool) {
	if clip.Metadata == nil {
		return "", false
	}
	marker, ok := clip.Metadata[preservedAudioLineageMetadataKey].(string)
	if !ok || lineageContext.prefix == "" {
		return "", false
	}
	lineageID, matches := strings.CutPrefix(marker, lineageContext.prefix+":")
	return lineageID, matches && lineageID != ""
}

func unlockPreservedIndependentAudio(
	document timeline.Document,
	preserved map[string]timeline.Track,
	operation map[string]any,
	lineageContext preservedAudioLineageContext,
) timeline.Document {
	copy := document
	copy.Tracks = append([]timeline.Track(nil), document.Tracks...)
	for trackIndex := range copy.Tracks {
		copy.Tracks[trackIndex] = copyTimelineTrack(copy.Tracks[trackIndex])
		if !ContainsString(independentAudioTrackIDs, copy.Tracks[trackIndex].TrackID) {
			continue
		}
		for clipIndex := range copy.Tracks[trackIndex].Clips {
			clip := &copy.Tracks[trackIndex].Clips[clipIndex]
			metadata := make(map[string]any, len(clip.Metadata)+1)
			for key, value := range clip.Metadata {
				metadata[key] = value
			}
			metadata[preservedAudioLineageMetadataKey] = lineageContext.markerValue(clip.TimelineClipID)
			if len(metadata) == 0 {
				clip.Metadata = nil
			} else {
				clip.Metadata = metadata
			}
		}
		if _, exists := preserved[copy.Tracks[trackIndex].TrackID]; exists {
			copy.Tracks[trackIndex].Locked = false
		}
	}
	probe, err := timeline.ApplyPatch(copy, operation)
	if err != nil {
		return document
	}
	for trackIndex := range copy.Tracks {
		track := document.Tracks[trackIndex]
		snapshot, exists := preserved[track.TrackID]
		if !exists || !track.Locked ||
			!onlyPreservedClipsChanged(track, probe.Tracks[trackIndex], snapshot, lineageContext) {
			copy.Tracks[trackIndex].Locked = track.Locked
		}
	}
	return copy
}

func onlyPreservedClipsChanged(
	current, result, preserved timeline.Track,
	lineageContext preservedAudioLineageContext,
) bool {
	preservedByID := make(map[string]timeline.Clip, len(preserved.Clips))
	for _, clip := range preserved.Clips {
		preservedByID[clip.TimelineClipID] = clip
	}
	currentUnprotected := make(map[string]timeline.Clip, len(current.Clips))
	for _, clip := range current.Clips {
		if _, protected := preservedByID[clip.TimelineClipID]; !protected {
			currentUnprotected[clip.TimelineClipID] = clip
		}
	}
	originalByID := make(map[string]timeline.Clip, len(current.Clips))
	for _, clip := range current.Clips {
		originalByID[clip.TimelineClipID] = clip
	}
	resultUnprotected := make(map[string]timeline.Clip, len(result.Clips))
	for _, clip := range result.Clips {
		if lineageID, marked := preservedAudioLineageID(clip, lineageContext); marked {
			if _, protected := preservedByID[lineageID]; protected {
				continue
			}
		}
		resultUnprotected[clip.TimelineClipID] = withoutPreservedAudioLineage(
			clip, originalByID, lineageContext,
		)
	}
	return reflect.DeepEqual(currentUnprotected, resultUnprotected)
}

func withoutPreservedAudioLineage(
	clip timeline.Clip,
	originalByID map[string]timeline.Clip,
	lineageContext preservedAudioLineageContext,
) timeline.Clip {
	lineageID, marked := preservedAudioLineageID(clip, lineageContext)
	if !marked {
		return clip
	}
	original, exists := originalByID[lineageID]
	if !exists {
		return clip
	}
	metadata := make(map[string]any, len(clip.Metadata))
	for key, value := range clip.Metadata {
		if key != preservedAudioLineageMetadataKey {
			metadata[key] = value
		}
	}
	if original.Metadata != nil {
		if value, hadOriginal := original.Metadata[preservedAudioLineageMetadataKey]; hadOriginal {
			metadata[preservedAudioLineageMetadataKey] = value
		}
	}
	if len(metadata) == 0 {
		clip.Metadata = nil
	} else {
		clip.Metadata = metadata
	}
	return clip
}

func stripPreservedAudioLineage(
	document *timeline.Document,
	current timeline.Document,
	lineageContext preservedAudioLineageContext,
) {
	originalByID := originalIndependentAudioClipsByID(current)
	for trackIndex := range document.Tracks {
		for clipIndex := range document.Tracks[trackIndex].Clips {
			clip := document.Tracks[trackIndex].Clips[clipIndex]
			if !shouldStripPreservedAudioLineage(clip, lineageContext) {
				continue
			}
			document.Tracks[trackIndex].Clips[clipIndex] = withoutPreservedAudioLineage(
				clip,
				originalByID,
				lineageContext,
			)
		}
	}
}

func stripPreservedAudioLineageFromTracks(
	tracks map[string]timeline.Track,
	current timeline.Document,
	lineageContext preservedAudioLineageContext,
) {
	originalByID := originalIndependentAudioClipsByID(current)
	for trackID, track := range tracks {
		track = copyTimelineTrack(track)
		for clipIndex := range track.Clips {
			if !shouldStripPreservedAudioLineage(track.Clips[clipIndex], lineageContext) {
				continue
			}
			track.Clips[clipIndex] = withoutPreservedAudioLineage(
				track.Clips[clipIndex], originalByID, lineageContext,
			)
		}
		tracks[trackID] = track
	}
}

func originalIndependentAudioClipsByID(document timeline.Document) map[string]timeline.Clip {
	originalByID := map[string]timeline.Clip{}
	for _, track := range document.Tracks {
		if !ContainsString(independentAudioTrackIDs, track.TrackID) {
			continue
		}
		for _, clip := range track.Clips {
			originalByID[clip.TimelineClipID] = clip
		}
	}
	return originalByID
}

func shouldStripPreservedAudioLineage(
	clip timeline.Clip,
	lineageContext preservedAudioLineageContext,
) bool {
	_, marked := preservedAudioLineageID(clip, lineageContext)
	return marked
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
	preserved map[string]timeline.Track,
	currentValid bool,
	lineageContext preservedAudioLineageContext,
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
		snapshot, protected := preserved[track.TrackID]
		_, trusted := trustedCurrent[track.TrackID]
		if independentAudioOverhangIsAuthorizedPerClip(
			previous,
			track,
			current.DurationFrames,
			result.DurationFrames,
			trusted,
			snapshot,
			protected,
			lineageContext,
		) {
			allowed[track.TrackID] = copyTimelineTrack(track)
		}
	}
	return allowed
}

func independentAudioOverhangIsAuthorizedPerClip(
	previous timeline.Track,
	result timeline.Track,
	previousDuration int,
	resultDuration int,
	trustedPrevious bool,
	preserved timeline.Track,
	hasPreserved bool,
	lineageContext preservedAudioLineageContext,
) bool {
	preservedByID := make(map[string]timeline.Clip, len(preserved.Clips))
	for _, clip := range preserved.Clips {
		preservedByID[clip.TimelineClipID] = clip
	}
	previousByID := make(map[string]timeline.Clip, len(previous.Clips))
	for _, clip := range previous.Clips {
		previousByID[clip.TimelineClipID] = clip
	}
	found := false
	for _, clip := range result.Clips {
		if clipOverhangFrames(clip, resultDuration) == 0 {
			continue
		}
		if origin, exists := preservedByID[clip.TimelineClipID]; hasPreserved && exists && reflect.DeepEqual(origin, clip) {
			found = true
			continue
		}
		if !trustedPrevious {
			return false
		}
		origin, exists := independentAudioClipAncestor(
			clip, previousByID, previous.Clips, lineageContext,
		)
		if !exists || clipOverhangFrames(clip, resultDuration) > clipOverhangFrames(origin, previousDuration) {
			return false
		}
		found = true
	}
	return found
}

func independentAudioClipAncestor(
	clip timeline.Clip,
	previousByID map[string]timeline.Clip,
	previous []timeline.Clip,
	lineageContext preservedAudioLineageContext,
) (timeline.Clip, bool) {
	if origin, exists := previousByID[clip.TimelineClipID]; exists {
		return origin, true
	}
	if lineageID, marked := preservedAudioLineageID(clip, lineageContext); marked {
		origin, exists := previousByID[lineageID]
		if exists && isSourceDescendant(clip, origin) {
			return origin, true
		}
		return timeline.Clip{}, false
	}
	if origin, exists := deterministicDerivedClipAncestor(clip, previousByID); exists {
		return origin, true
	}
	return uniqueSourceAncestor(clip, previous)
}

func deterministicDerivedClipAncestor(
	clip timeline.Clip,
	previous map[string]timeline.Clip,
) (timeline.Clip, bool) {
	var ancestor timeline.Clip
	found := false
	for clipID, candidate := range previous {
		for _, marker := range []string{"_after_", "_split_"} {
			suffix, matches := strings.CutPrefix(clip.TimelineClipID, clipID+marker)
			if !matches || suffix == "" {
				continue
			}
			if _, err := strconv.Atoi(suffix); err != nil || !isSourceDescendant(clip, candidate) {
				continue
			}
			if found {
				return timeline.Clip{}, false
			}
			ancestor = candidate
			found = true
		}
	}
	return ancestor, found
}

func uniqueSourceAncestor(
	clip timeline.Clip,
	previous []timeline.Clip,
) (timeline.Clip, bool) {
	var ancestor timeline.Clip
	found := false
	for _, candidate := range previous {
		if !isSourceDescendant(clip, candidate) {
			continue
		}
		if found {
			return timeline.Clip{}, false
		}
		ancestor = candidate
		found = true
	}
	return ancestor, found
}

func isSourceDescendant(clip, candidate timeline.Clip) bool {
	return candidate.TrackID == clip.TrackID && candidate.AssetID == clip.AssetID &&
		candidate.AssetKind == clip.AssetKind && candidate.Role == clip.Role &&
		candidate.PlaybackRate == clip.PlaybackRate &&
		clip.SourceStartFrame >= candidate.SourceStartFrame &&
		clip.SourceEndFrame <= candidate.SourceEndFrame
}

func clipOverhangFrames(clip timeline.Clip, durationFrames int) int {
	return max(0, clip.TimelineEndFrame-durationFrames)
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
