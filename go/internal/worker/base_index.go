package worker

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

const baseIndexJobPriority = 80

// EnqueueBaseShotIndexBackfill repairs old workspaces without delaying worker
// startup on VLM work. It only publishes reducer events; the normal bounded
// worker lanes execute the jobs and RecoverStale handles process restarts.
func EnqueueBaseShotIndexBackfill(ctx context.Context, database *storage.DB) error {
	rows, err := database.Read().QueryContext(ctx, `
		SELECT asset_id FROM assets
		WHERE kind='video' AND ingest_status='ready' AND usable=1
		ORDER BY asset_id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	assetIDs := []string{}
	for rows.Next() {
		var assetID string
		if err := rows.Scan(&assetID); err != nil {
			return err
		}
		assetIDs = append(assetIDs, assetID)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, assetID := range assetIDs {
		if err := ensureBaseShotIndexForAsset(
			ctx, database, assetID, "", reducer.Options{Actor: contracts.ActorJob},
		); err != nil {
			return fmt.Errorf("回填素材 %s 的基础镜头索引: %w", assetID, err)
		}
	}
	return nil
}

func ensureBaseShotIndexForAsset(
	ctx context.Context,
	database *storage.DB,
	assetID string,
	draftID string,
	options reducer.Options,
) error {
	asset, err := storage.GetAsset(ctx, database.Read(), assetID)
	if err != nil {
		return err
	}
	if asset.Kind != "video" || asset.IngestStatus != "ready" || !asset.Usable {
		return nil
	}
	if snapshot, readyErr := storage.ReadyShotIndexByContentHash(
		ctx, database.Read(), asset.Hash,
	); readyErr == nil {
		if isCurrentBaseShotIndex(snapshot) {
			return materializeReadyBaseIndex(ctx, database, asset, snapshot, options)
		}
	} else if !errors.Is(readyErr, storage.ErrNotFound) {
		return readyErr
	}
	if draftID == "" {
		_ = database.Read().QueryRowContext(ctx, `
			SELECT draft_id FROM draft_asset_links WHERE asset_id=?
			ORDER BY linked_at,draft_id LIMIT 1`, asset.ID).Scan(&draftID)
	}
	fingerprint := understanding.BaseIndexFingerprint(asset)
	idempotencyKey := understanding.BaseIndexIdempotencyKey(fingerprint)
	var existingJobID string
	if err := database.Read().QueryRowContext(ctx, `
		SELECT job_id FROM jobs WHERE kind='understand' AND idempotency_key=?`,
		idempotencyKey,
	).Scan(&existingJobID); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	jobID := deterministicJobID(idempotencyKey)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	event := contracts.Event{Type: "JobEnqueued", DraftID: draftID, Payload: map[string]any{
		"job_id": jobID, "kind": "understand", "asset_id": asset.ID,
		"requested_by_draft_id": draftID, "idempotency_key": idempotencyKey,
		"job_payload": map[string]any{
			"asset_id": asset.ID, "focus": "", "depth": "scan", "max_steps_per_asset": 0,
			"force_refresh": false, "refresh_nonce": "",
			"analysis_fingerprint": fingerprint,
		},
		"max_retries": 2, "next_run_at": now, "priority": baseIndexJobPriority,
	}}
	result, applyErr := reducer.Apply(ctx, database, []contracts.Event{event}, options)
	if applyErr == nil && result.Status == reducer.StatusApplied {
		return nil
	}
	// Two ingest lanes may discover the same content hash concurrently. The
	// unique job identity is the singleflight fence; once the winner is visible,
	// the loser has also achieved the desired state.
	if err := database.Read().QueryRowContext(ctx, `
		SELECT job_id FROM jobs WHERE kind='understand' AND idempotency_key=?`,
		idempotencyKey,
	).Scan(&existingJobID); err == nil {
		return nil
	}
	return errors.Join(applyErr, fmt.Errorf("基础镜头索引入队状态: %s", result.Status))
}

func isCurrentBaseShotIndex(snapshot storage.ShotIndexSnapshot) bool {
	return snapshot.OutputSchemaVersion >= understanding.BaseShotIndexSchemaVersion &&
		snapshot.AnalyzerVersion == understanding.PromptVersion
}

func materializeReadyBaseIndex(
	ctx context.Context,
	database *storage.DB,
	asset storage.Asset,
	snapshot storage.ShotIndexSnapshot,
	options reducer.Options,
) error {
	summaryID := materializedSummaryID(asset.ID, snapshot.ID)
	var exists int
	if err := database.Read().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM material_summaries WHERE summary_id=? AND status='ready'`, summaryID,
	).Scan(&exists); err != nil {
		return err
	}
	sourceAlreadyMaterialized := snapshot.SourceAssetID != nil && *snapshot.SourceAssetID == asset.ID
	if asset.UnderstandingStatus == "ready" && (exists > 0 || sourceAlreadyMaterialized) {
		return nil
	}
	summary := cloneMap(snapshot.Summary)
	summary["asset_id"] = asset.ID
	model := stringValue(summary["model"])
	rows := reducer.ResultRows{}
	if exists == 0 {
		rows.MaterialSummaries = []reducer.MaterialSummaryRow{{
			ID: summaryID, AssetID: asset.ID, Status: "ready", Summary: summary,
			Model: stringPointer(model), Fingerprint: stringPointer(understanding.BaseIndexFingerprint(asset)),
			PromptVersion: stringPointer(understanding.PromptVersion),
		}}
	}
	result, err := reducer.Apply(ctx, database, []contracts.Event{{
		Type:    "MaterialUnderstandingCompleted",
		Payload: map[string]any{"asset_id": asset.ID, "index_snapshot_id": snapshot.ID},
	}}, withResultRows(options, rows))
	if err != nil {
		return err
	}
	if result.Status != reducer.StatusApplied {
		return fmt.Errorf("复用基础镜头索引 reducer status: %s", result.Status)
	}
	return nil
}

func publishBaseShotIndex(
	ctx context.Context,
	database *storage.DB,
	job Job,
	asset storage.Asset,
	summary understanding.Summary,
	fingerprint string,
	summaryID string,
) (storage.ShotIndexSnapshot, int, error) {
	summary = understanding.WithSemanticNames(summary)
	generation := 1
	previous := []storage.IndexedShot{}
	if ready, err := storage.ReadyShotIndexByContentHash(ctx, database.Read(), asset.Hash); err == nil {
		generation = ready.Generation + 1
		previous, err = storage.ListShotIndexShots(ctx, database.Read(), ready.ID)
		if err != nil {
			return storage.ShotIndexSnapshot{}, 0, err
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		return storage.ShotIndexSnapshot{}, 0, err
	} else {
		_ = database.Read().QueryRowContext(ctx, `
			SELECT COALESCE(MAX(generation),0)+1 FROM shot_index_snapshots
			WHERE asset_content_hash=?`, asset.Hash).Scan(&generation)
	}
	shots, err := understanding.BuildBaseIndexShots(asset.Hash, generation, summary, previous)
	if err != nil {
		return storage.ShotIndexSnapshot{}, 0, err
	}
	snapshotID := understanding.BaseIndexSnapshotID(asset.Hash, fingerprint, generation)
	summaryMap := summaryToMap(summary)
	snapshotSummary := cloneMap(summaryMap)
	delete(snapshotSummary, "asset_id")
	shotRows := make([]reducer.ShotIndexShotRow, 0, len(shots))
	frameCount := 0
	for _, shot := range shots {
		frames := make([]map[string]any, 0, len(shot.RepresentativeFrames))
		for _, frame := range shot.RepresentativeFrames {
			frames = append(frames, map[string]any{
				"source_frame": frame.SourceFrame, "timestamp_ms": frame.TimestampMS,
				"position": frame.Position, "object_hash": frame.ObjectHash,
				"object_size": frame.ObjectSize,
			})
		}
		frameCount += len(frames)
		shotRows = append(shotRows, reducer.ShotIndexShotRow{
			ID: shot.ShotID, AssetContentHash: asset.Hash,
			SourceStartFrame: shot.SourceStartFrame, SourceEndFrame: shot.SourceEndFrame,
			BoundaryVersion: shot.BoundaryVersion, BoundaryKind: shot.BoundaryKind,
			BoundaryConfidence:  shot.BoundaryConfidence,
			LineageParentShotID: shot.LineageParentShotID, RepresentativeFrames: frames,
			SemanticName: shot.SemanticName, Description: shot.Description,
			Tags: shot.Tags, Subjects: shot.Subjects,
			Actions: shot.Actions, Setting: shot.Setting, ShotScale: shot.ShotScale,
			Composition: shot.Composition, Lighting: shot.Lighting, Mood: shot.Mood,
			EditHints: shot.EditHints, Quality: shot.Quality, SearchText: shot.SearchText,
			SearchTokens: shot.SearchTokens, DeepCoverage: []string{},
		})
	}
	model := summary.Model
	assetRows, err := database.Read().QueryContext(ctx, `
		SELECT asset_id FROM assets
		WHERE hash=? AND kind='video' AND ingest_status='ready' AND usable=1
		ORDER BY asset_id`, asset.Hash)
	if err != nil {
		return storage.ShotIndexSnapshot{}, 0, err
	}
	contentAssetIDs := []string{}
	for assetRows.Next() {
		var contentAssetID string
		if err := assetRows.Scan(&contentAssetID); err != nil {
			_ = assetRows.Close()
			return storage.ShotIndexSnapshot{}, 0, err
		}
		contentAssetIDs = append(contentAssetIDs, contentAssetID)
	}
	if err := errors.Join(assetRows.Err(), assetRows.Close()); err != nil {
		return storage.ShotIndexSnapshot{}, 0, err
	}
	events := make([]contracts.Event, 0, len(contentAssetIDs))
	summaryRows := make([]reducer.MaterialSummaryRow, 0, len(contentAssetIDs))
	for _, contentAssetID := range contentAssetIDs {
		contentSummary := cloneMap(summaryMap)
		contentSummary["asset_id"] = contentAssetID
		contentSummaryID := materializedSummaryID(contentAssetID, snapshotID)
		if contentAssetID == asset.ID {
			contentSummaryID = summaryID
		}
		events = append(events, contracts.Event{
			Type: "MaterialUnderstandingCompleted", Payload: map[string]any{
				"asset_id": contentAssetID, "job_id": job.ID, "attempt": job.Attempts,
				"summary_id": contentSummaryID, "index_snapshot_id": snapshotID,
			},
		})
		summaryRows = append(summaryRows, reducer.MaterialSummaryRow{
			ID: contentSummaryID, AssetID: contentAssetID, Status: "ready", Summary: contentSummary,
			Model: stringPointer(model), Fingerprint: stringPointer(fingerprint),
			PromptVersion: stringPointer(understanding.PromptVersion),
		})
	}
	result, err := reducer.Apply(ctx, database, events,
		claimedJobOptions(job, reducer.Options{ResultRows: reducer.ResultRows{
			MaterialSummaries: summaryRows,
			ShotIndexSnapshots: []reducer.ShotIndexSnapshotRow{{
				ID: snapshotID, AssetContentHash: asset.Hash, Generation: generation,
				AnalyzerVersion:     understanding.PromptVersion,
				OutputSchemaVersion: understanding.BaseShotIndexSchemaVersion,
				SourceAssetID:       asset.ID, Summary: snapshotSummary, Shots: shotRows,
			}},
		}}))
	if err != nil || result.Status != reducer.StatusApplied {
		return storage.ShotIndexSnapshot{}, 0,
			errors.Join(err, fmt.Errorf("shot index reducer status: %s", result.Status))
	}
	snapshot, err := storage.ShotIndexSnapshotByID(ctx, database.Read(), snapshotID)
	return snapshot, frameCount, err
}

func deterministicJobID(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return "job_" + hex.EncodeToString(sum[:12])
}

func materializedSummaryID(assetID, snapshotID string) string {
	sum := sha256.Sum256([]byte(assetID + "\x00" + snapshotID))
	return "summary_" + hex.EncodeToString(sum[:12])
}

func summaryToMap(summary understanding.Summary) map[string]any {
	data, _ := json.Marshal(summary)
	result := map[string]any{}
	_ = json.Unmarshal(data, &result)
	return result
}

func cloneMap(value map[string]any) map[string]any {
	data, _ := json.Marshal(value)
	result := map[string]any{}
	_ = json.Unmarshal(data, &result)
	return result
}

func stringPointer(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func withResultRows(options reducer.Options, rows reducer.ResultRows) reducer.Options {
	options.ResultRows = rows
	return options
}
