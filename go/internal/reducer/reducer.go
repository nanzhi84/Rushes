package reducer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

type Status string

const (
	StatusApplied          Status = "applied"
	StatusVersionConflict  Status = "version_conflict"
	StatusValidationFailed Status = "validation_failed"
)

var (
	ErrJobCancelled      = errors.New("job 已取消")
	ErrJobNotCancellable = errors.New("job 当前状态不可取消")
	ErrJobClaimLost      = errors.New("job 已被其他 worker 重新领取")
)

type VersionConflict struct {
	DraftID             string `json:"draft_id"`
	ExpectedBaseVersion *int   `json:"expected_base_version"`
	ActualStateVersion  int    `json:"actual_state_version"`
	EventType           string `json:"event_type"`
}

type AppliedEvent struct {
	ID           int64
	Type         string
	StateVersion *int
}

type Result struct {
	Status             Status
	AppliedEvents      []AppliedEvent
	DraftStateVersions map[string]int
	Conflict           *VersionConflict
	SkippedEvents      int
}

type MessageRow struct {
	ID        string
	DraftID   string
	Role      string
	Kind      string
	Content   string
	CreatedAt string
}

type MaterialSummaryRow struct {
	ID            string
	AssetID       string
	Version       int // 小于 1 时在当前 reducer 事务内按 asset 分配下一版本。
	Focus         *string
	Status        string
	Summary       map[string]any
	Model         *string
	Fingerprint   *string
	PromptVersion *string
	CreatedAt     string
}

type TranscriptRow struct {
	ID           string
	AssetID      string
	ProviderID   string
	RawPreserved bool
	Utterances   []map[string]any
	VADSegments  []map[string]any
}

// AgentContextCheckpointRow persists the model's replacement-history window.
// It is deliberately a reducer result row rather than a domain event: changing
// prompt bookkeeping must not pretend that the user's video state changed.
type AgentContextCheckpointRow struct {
	DraftID                   string
	WindowID                  string
	WindowNumber              int
	HistoryVersion            int
	BaseSnapshot              map[string]any
	BaseSnapshotHash          string
	Summary                   string
	CompactedThroughMessageID *string
}

// DraftPlanUpdateRow persists the model's private cross-turn creative plan.
// Like AgentContextCheckpointRow it is bookkeeping, not a domain event: updating
// it must not bump draft state_version or emit domain SSE events.
type DraftPlanUpdateRow struct {
	DraftID     string
	ContentPlan map[string]any
}

type ResultRows struct {
	Message                *MessageRow
	MaterialSummaries      []MaterialSummaryRow
	Transcripts            []TranscriptRow
	AgentContextCheckpoint *AgentContextCheckpointRow
	DraftPlanUpdate        *DraftPlanUpdateRow
}

type ValidationHook func(context.Context, *sql.Tx, []string) error

type Options struct {
	BaseVersion *int
	Actor       contracts.Actor
	CreatedAt   time.Time
	ResultRows  ResultRows
	Validate    ValidationHook
}

type applyState struct {
	tx               *sql.Tx
	createdAt        string
	originalVersions map[string]int
	touched          map[string]struct{}
	eventsToLog      []contracts.Event
	skipped          int
}

func Apply(
	ctx context.Context,
	database *storage.DB,
	events []contracts.Event,
	options Options,
) (Result, error) {
	if !options.Actor.Valid() {
		return Result{}, errors.New("reducer actor 无效")
	}
	if len(events) == 0 && emptyResultRows(options.ResultRows) {
		return Result{Status: StatusApplied, DraftStateVersions: map[string]int{}}, nil
	}
	createdAt := options.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	normalized := make([]contracts.Event, 0, len(events))
	for _, event := range events {
		event.Actor = options.Actor
		event.CreatedAt = createdAt.Format(time.RFC3339Nano)
		spec, ok := event.Spec()
		if !ok {
			return Result{}, fmt.Errorf("reducer 收到未注册事件 %q", event.Type)
		}
		if spec.Mode == contracts.VersionStrict && event.BaseVersion == nil {
			event.BaseVersion = options.BaseVersion
		}
		if spec.Mode == contracts.VersionMerge {
			event.BaseVersion = nil
		}
		if err := event.Validate(); err != nil {
			return Result{}, err
		}
		normalized = append(normalized, event)
	}

	tx, err := database.Write().BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	state := &applyState{
		tx:               tx,
		createdAt:        createdAt.Format(time.RFC3339Nano),
		originalVersions: map[string]int{},
		touched:          map[string]struct{}{},
	}

	if conflict, err := preflightStrict(ctx, state, normalized); err != nil {
		return Result{}, err
	} else if conflict != nil {
		return Result{Status: StatusVersionConflict, Conflict: conflict}, nil
	}

	for _, event := range normalized {
		duplicate, err := isDuplicateMerge(ctx, state.tx, event)
		if err != nil {
			return Result{}, err
		}
		if duplicate {
			state.skipped++
			continue
		}
		if err := applyEvent(ctx, state, event); err != nil {
			return Result{}, err
		}
		state.eventsToLog = append(state.eventsToLog, event)
	}

	touched := sortedKeys(state.touched)
	if options.Validate != nil {
		if err := options.Validate(ctx, tx, touched); err != nil {
			return Result{Status: StatusValidationFailed}, nil
		}
	}
	if err := persistResultRows(ctx, tx, options.ResultRows, state.createdAt); err != nil {
		return Result{}, err
	}

	versions := map[string]int{}
	for _, draftID := range touched {
		expected := state.originalVersions[draftID]
		result, err := tx.ExecContext(ctx, `
			UPDATE drafts SET state_version=state_version+1, updated_at=?
			WHERE draft_id=? AND state_version=?`, state.createdAt, draftID, expected)
		if err != nil {
			return Result{}, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return Result{}, err
		}
		if rows != 1 {
			actual, lookupErr := lookupDraftVersion(ctx, tx, draftID)
			if lookupErr != nil {
				return Result{}, lookupErr
			}
			return Result{
				Status: StatusVersionConflict,
				Conflict: &VersionConflict{
					DraftID: draftID, ExpectedBaseVersion: intPointer(expected),
					ActualStateVersion: actual, EventType: "DraftStateUpdate",
				},
			}, nil
		}
		versions[draftID] = expected + 1
	}

	applied, err := appendEvents(ctx, state, versions)
	if err != nil {
		return Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return Result{}, err
	}
	committed = true
	return Result{
		Status: StatusApplied, AppliedEvents: applied, DraftStateVersions: versions,
		SkippedEvents: state.skipped,
	}, nil
}

func preflightStrict(
	ctx context.Context,
	state *applyState,
	events []contracts.Event,
) (*VersionConflict, error) {
	for _, event := range events {
		spec, _ := event.Spec()
		if spec.Mode != contracts.VersionStrict {
			continue
		}
		if event.DraftID == "" {
			return nil, fmt.Errorf("strict 事件 %s 缺少 draft_id", event.Type)
		}
		actual, err := state.version(ctx, event.DraftID)
		if err != nil {
			return nil, err
		}
		if event.BaseVersion == nil || *event.BaseVersion != actual {
			return &VersionConflict{
				DraftID: event.DraftID, ExpectedBaseVersion: event.BaseVersion,
				ActualStateVersion: actual, EventType: event.Type,
			}, nil
		}
	}
	return nil, nil
}

func (state *applyState) version(ctx context.Context, draftID string) (int, error) {
	if version, ok := state.originalVersions[draftID]; ok {
		return version, nil
	}
	version, err := lookupDraftVersion(ctx, state.tx, draftID)
	if err != nil {
		return 0, err
	}
	state.originalVersions[draftID] = version
	return version, nil
}

func (state *applyState) touch(ctx context.Context, draftID string) error {
	if _, err := state.version(ctx, draftID); err != nil {
		return err
	}
	state.touched[draftID] = struct{}{}
	return nil
}

func lookupDraftVersion(ctx context.Context, query storage.Querier, draftID string) (int, error) {
	var version int
	if err := query.QueryRowContext(ctx,
		"SELECT state_version FROM drafts WHERE draft_id=?", draftID,
	).Scan(&version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("草稿不存在: %s", draftID)
		}
		return 0, err
	}
	return version, nil
}

func isDuplicateMerge(ctx context.Context, tx *sql.Tx, event contracts.Event) (bool, error) {
	key, err := event.MergeKey()
	if err != nil {
		return false, err
	}
	if key == "" {
		return false, nil
	}
	var found int
	err = tx.QueryRowContext(ctx,
		"SELECT 1 FROM event_log WHERE event_type=? AND merge_key=? LIMIT 1", event.Type, key,
	).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func applyEvent(ctx context.Context, state *applyState, event contracts.Event) error {
	switch event.Type {
	case "DraftCreated":
		return applyDraftCreated(ctx, state, event)
	case "DraftRenamed", "DraftCopied", "DraftTrashed":
		return applyDraftLifecycle(ctx, state, event)
	case "AssetImported", "AssetProbed", "ProxyGenerated",
		"MaterialUnderstandingStarted", "MaterialUnderstandingCompleted", "MaterialUnderstandingFailed":
		return applyAssetEvent(ctx, state, event)
	case "AssetLinked":
		return applyAssetLinked(ctx, state, event)
	case "AssetUnlinked":
		return applyAssetUnlinked(ctx, state, event)
	case "DecisionCreated":
		return applyDecisionCreated(ctx, state, event)
	case "DecisionAnswered":
		return applyDecisionAnswered(ctx, state, event)
	case "ConversationContextCleared":
		return applyConversationContextCleared(ctx, state, event)
	case "TimelineVersionCreated":
		return applyTimelineCreated(ctx, state, event)
	case "TimelineValidated", "TimelineValidationFailed":
		return applyTimelineValidation(ctx, state, event)
	case "PreviewRendered":
		return applyPreviewRendered(ctx, state, event)
	case "PreviewViewed":
		return applyPreviewViewed(ctx, state, event)
	case "ExportCompleted":
		return applyExportCompleted(ctx, state, event)
	case "JobEnqueued", "JobProgress", "JobSucceeded", "JobFailed", "JobCancelled":
		return applyJob(ctx, state, event)
	default:
		return fmt.Errorf("reducer 未实现事件 %s", event.Type)
	}
}

func applyDraftCreated(ctx context.Context, state *applyState, event contracts.Event) error {
	defaults := mapValue(event.Payload["defaults"])
	brief := mapValue(event.Payload["brief"])
	if len(brief) == 0 {
		brief = map[string]any{"goal": stringFrom(event.Payload["goal"], "")}
	}
	_, err := state.tx.ExecContext(ctx, `
		INSERT INTO drafts(
			draft_id, name, state_version, status, defaults_json, running_jobs_json,
			brief_json, timeline_validated, scratch_memory_json, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, '[]', ?, 0, '{}', ?, ?)`,
		event.DraftID,
		stringFrom(event.Payload["name"], "未命名草稿"),
		intFrom(event.Payload["state_version"], 0),
		stringFrom(event.Payload["status"], "active"),
		mustJSON(defaults), mustJSON(brief), state.createdAt, state.createdAt,
	)
	if err == nil {
		state.originalVersions[event.DraftID] = intFrom(event.Payload["state_version"], 0)
	}
	return err
}

func applyDraftLifecycle(ctx context.Context, state *applyState, event contracts.Event) error {
	if event.Type == "DraftCopied" {
		return applyDraftCopied(ctx, state, event)
	}
	if err := state.touch(ctx, event.DraftID); err != nil {
		return err
	}
	switch event.Type {
	case "DraftRenamed":
		_, err := state.tx.ExecContext(ctx, "UPDATE drafts SET name=? WHERE draft_id=?",
			stringFrom(event.Payload["name"], "未命名草稿"), event.DraftID)
		return err
	case "DraftTrashed":
		_, err := state.tx.ExecContext(ctx, "UPDATE drafts SET status='trashed' WHERE draft_id=?", event.DraftID)
		return err
	default:
		return fmt.Errorf("未实现草稿生命周期事件 %s", event.Type)
	}
}

func applyDraftCopied(ctx context.Context, state *applyState, event contracts.Event) error {
	sourceDraftID := stringFrom(event.Payload["source_draft_id"], "")
	if sourceDraftID == "" {
		return errors.New("DraftCopied 缺少 source_draft_id")
	}
	var sourceName string
	var sourcePreview, sourceViewedPreview, sourceExport sql.NullString
	if err := state.tx.QueryRowContext(ctx, `
		SELECT name, preview_current_id, last_viewed_preview_id, export_current_id
		FROM drafts WHERE draft_id=?`, sourceDraftID,
	).Scan(&sourceName, &sourcePreview, &sourceViewedPreview, &sourceExport); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("草稿不存在: %s", sourceDraftID)
		}
		return err
	}
	name := stringFrom(event.Payload["name"], sourceName+" Copy")
	result, err := state.tx.ExecContext(ctx, `
		INSERT INTO drafts(
			draft_id, name, state_version, status, defaults_json, pending_decision_id,
			running_jobs_json, last_error_json, brief_json, content_plan_json,
			timeline_current_version, timeline_validated, preview_current_id,
			last_viewed_preview_id, export_current_id, scratch_memory_json,
			messages_tail_ref, created_at, updated_at
		)
		SELECT ?, ?, 0, 'active', defaults_json, NULL, '[]', NULL, brief_json,
			content_plan_json, timeline_current_version, timeline_validated, NULL,
			NULL, NULL, scratch_memory_json, NULL, ?, ?
		FROM drafts WHERE draft_id=?`,
		event.DraftID, name, state.createdAt, state.createdAt, sourceDraftID,
	)
	if err != nil {
		return err
	}
	if rows, rowsErr := result.RowsAffected(); rowsErr != nil || rows != 1 {
		return errors.Join(rowsErr, fmt.Errorf("复制草稿失败: %s", sourceDraftID))
	}
	state.originalVersions[event.DraftID] = 0
	if _, err := state.tx.ExecContext(ctx, `
		INSERT INTO draft_asset_links(draft_id, asset_id, linked_at, note, rel_dir)
		SELECT ?, asset_id, ?, note, rel_dir FROM draft_asset_links WHERE draft_id=?`,
		event.DraftID, state.createdAt, sourceDraftID,
	); err != nil {
		return err
	}
	if err := copyDraftTimelines(ctx, state, sourceDraftID, event.DraftID); err != nil {
		return err
	}
	previewIDs, err := copyDraftPreviews(ctx, state, sourceDraftID, event.DraftID)
	if err != nil {
		return err
	}
	exportIDs, err := copyDraftExports(ctx, state, sourceDraftID, event.DraftID)
	if err != nil {
		return err
	}
	_, err = state.tx.ExecContext(ctx, `
		UPDATE drafts SET preview_current_id=?, last_viewed_preview_id=?, export_current_id=?
		WHERE draft_id=?`, copiedReference(sourcePreview, previewIDs),
		copiedReference(sourceViewedPreview, previewIDs), copiedReference(sourceExport, exportIDs), event.DraftID)
	return err
}

func copyDraftTimelines(ctx context.Context, state *applyState, sourceDraftID, targetDraftID string) error {
	rows, err := state.tx.QueryContext(ctx, `
		SELECT version, created_by_patch_id, document_json, validation_report_json
		FROM timeline_versions
		WHERE draft_id=? AND version=(
			SELECT timeline_current_version FROM drafts WHERE draft_id=?
		)`, sourceDraftID, sourceDraftID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var version int
		var patch, validation sql.NullString
		var raw string
		if err := rows.Scan(&version, &patch, &raw, &validation); err != nil {
			return err
		}
		document := map[string]any{}
		if err := json.Unmarshal([]byte(raw), &document); err != nil {
			return err
		}
		timelineID := fmt.Sprintf("%s:v%d", targetDraftID, version)
		document["draft_id"] = targetDraftID
		document["timeline_id"] = timelineID
		if _, err := state.tx.ExecContext(ctx, `
			INSERT INTO timeline_versions(
				timeline_id, draft_id, version, created_by_patch_id,
				document_json, validation_report_json, created_at
			) VALUES(?, ?, ?, ?, ?, ?, ?)`, timelineID, targetDraftID, version,
			patch, mustJSON(document), validation, state.createdAt,
		); err != nil {
			return err
		}
	}
	return rows.Err()
}

func copyDraftPreviews(
	ctx context.Context,
	state *applyState,
	sourceDraftID, targetDraftID string,
) (map[string]string, error) {
	rows, err := state.tx.QueryContext(ctx, `
		SELECT preview_id, timeline_version, object_hash, quality_json,
			render_width, render_height, render_fps, expected_duration_sec
		FROM previews WHERE draft_id=?`, sourceDraftID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	identifiers := map[string]string{}
	for rows.Next() {
		var sourceID, objectHash, quality string
		var timelineVersion int
		var width, height sql.NullInt64
		var fps, duration sql.NullFloat64
		if err := rows.Scan(&sourceID, &timelineVersion, &objectHash, &quality,
			&width, &height, &fps, &duration); err != nil {
			return nil, err
		}
		targetID := targetDraftID + ":" + sourceID
		if _, err := state.tx.ExecContext(ctx, `
			INSERT INTO previews(
				preview_id, draft_id, timeline_version, object_hash, quality_json,
				render_width, render_height, render_fps, expected_duration_sec, created_at
			) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, targetID, targetDraftID,
			timelineVersion, objectHash, quality, width, height, fps, duration, state.createdAt,
		); err != nil {
			return nil, err
		}
		identifiers[sourceID] = targetID
	}
	return identifiers, rows.Err()
}

func copyDraftExports(
	ctx context.Context,
	state *applyState,
	sourceDraftID, targetDraftID string,
) (map[string]string, error) {
	rows, err := state.tx.QueryContext(ctx, `
		SELECT export_id, timeline_version, object_hash, quality_json
		FROM exports WHERE draft_id=?`, sourceDraftID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	identifiers := map[string]string{}
	for rows.Next() {
		var sourceID, objectHash, quality string
		var timelineVersion int
		if err := rows.Scan(&sourceID, &timelineVersion, &objectHash, &quality); err != nil {
			return nil, err
		}
		targetID := targetDraftID + ":" + sourceID
		if _, err := state.tx.ExecContext(ctx, `
			INSERT INTO exports(export_id, draft_id, timeline_version, object_hash, quality_json, created_at)
			VALUES(?, ?, ?, ?, ?, ?)`, targetID, targetDraftID, timelineVersion,
			objectHash, quality, state.createdAt,
		); err != nil {
			return nil, err
		}
		identifiers[sourceID] = targetID
	}
	return identifiers, rows.Err()
}

func copiedReference(source sql.NullString, identifiers map[string]string) any {
	if !source.Valid {
		return nil
	}
	return nullableText(identifiers[source.String])
}

func applyAssetEvent(ctx context.Context, state *applyState, event contracts.Event) error {
	assetID := stringFrom(event.Payload["asset_id"], "")
	if assetID == "" {
		return fmt.Errorf("%s 缺少 asset_id", event.Type)
	}
	if event.Type == "AssetImported" {
		if hash := stringFrom(event.Payload["object_hash"], ""); hash != "" {
			if err := ensureObject(ctx, state, hash, int64From(event.Payload["object_size"], int64From(event.Payload["size"], 0))); err != nil {
				return err
			}
		}
		_, err := state.tx.ExecContext(ctx, `
			INSERT INTO assets(
				asset_id, storage_mode, object_hash, reference_path, kind, source, filename,
				hash, mtime, size, probe_json, proxy_object_hash, thumbnail_object_hash,
				ingest_status, understanding_status, usable, failure_json
			) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			assetID,
			stringFrom(event.Payload["storage_mode"], "reference"), nullableString(event.Payload["object_hash"]),
			nullableString(event.Payload["reference_path"]), stringFrom(event.Payload["kind"], "video"),
			stringFrom(event.Payload["source"], "local"), stringFrom(event.Payload["filename"], ""),
			stringFrom(event.Payload["hash"], assetID), nullableInt64(event.Payload["mtime"]),
			int64From(event.Payload["size"], 0), nullableJSON(event.Payload["probe"]),
			nullableString(event.Payload["proxy_object_hash"]), nullableString(event.Payload["thumbnail_object_hash"]),
			stringFrom(event.Payload["ingest_status"], "imported"),
			stringFrom(event.Payload["understanding_status"], "none"), boolInt(boolFrom(event.Payload["usable"], true)),
			nullableJSON(event.Payload["failure"]),
		)
		return err
	}
	switch event.Type {
	case "AssetProbed":
		thumbnailHash := stringFrom(event.Payload["thumbnail_object_hash"], "")
		if thumbnailHash != "" {
			if err := ensureObject(ctx, state, thumbnailHash, int64From(event.Payload["thumbnail_object_size"], 0)); err != nil {
				return err
			}
		}
		_, err := state.tx.ExecContext(ctx,
			`UPDATE assets SET probe_json=?, thumbnail_object_hash=COALESCE(?, thumbnail_object_hash),
			ingest_status=?, usable=1, failure_json=NULL WHERE asset_id=?`,
			mustJSON(mapValue(event.Payload["probe"])), nullableText(thumbnailHash),
			stringFrom(event.Payload["ingest_status"], "probed"), assetID)
		return err
	case "ProxyGenerated":
		hash := stringFrom(event.Payload["proxy_object_hash"], "")
		if hash == "" {
			return errors.New("ProxyGenerated 缺少 proxy_object_hash")
		}
		if err := ensureObject(ctx, state, hash, int64From(event.Payload["proxy_object_size"], 0)); err != nil {
			return err
		}
		_, err := state.tx.ExecContext(ctx,
			"UPDATE assets SET proxy_object_hash=?, ingest_status=? WHERE asset_id=?",
			hash, stringFrom(event.Payload["ingest_status"], "ready"), assetID)
		return err
	case "MaterialUnderstandingStarted":
		_, err := state.tx.ExecContext(ctx, "UPDATE assets SET understanding_status='running' WHERE asset_id=?", assetID)
		return err
	case "MaterialUnderstandingCompleted":
		_, err := state.tx.ExecContext(ctx, "UPDATE assets SET understanding_status='ready' WHERE asset_id=?", assetID)
		return err
	case "MaterialUnderstandingFailed":
		if boolFrom(event.Payload["cancelled"], false) {
			_, err := state.tx.ExecContext(ctx,
				"UPDATE assets SET understanding_status='none', failure_json=NULL WHERE asset_id=?", assetID)
			return err
		}
		_, err := state.tx.ExecContext(ctx, "UPDATE assets SET understanding_status='failed', failure_json=? WHERE asset_id=?",
			nullableJSON(event.Payload["failure"]), assetID)
		return err
	}
	return nil
}

func applyAssetLinked(ctx context.Context, state *applyState, event contracts.Event) error {
	assetID := stringFrom(event.Payload["asset_id"], "")
	_, err := state.tx.ExecContext(ctx, `
		INSERT INTO draft_asset_links(draft_id, asset_id, linked_at, note, rel_dir)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(draft_id, asset_id) DO UPDATE SET note=excluded.note, rel_dir=excluded.rel_dir`,
		event.DraftID, assetID, state.createdAt, stringFrom(event.Payload["note"], ""),
		nullableString(event.Payload["rel_dir"]),
	)
	return err
}

func applyAssetUnlinked(ctx context.Context, state *applyState, event contracts.Event) error {
	if err := state.touch(ctx, event.DraftID); err != nil {
		return err
	}
	result, err := state.tx.ExecContext(ctx,
		"DELETE FROM draft_asset_links WHERE draft_id=? AND asset_id=?",
		event.DraftID, stringFrom(event.Payload["asset_id"], ""))
	if err != nil {
		return err
	}
	if rows, rowsErr := result.RowsAffected(); rowsErr != nil || rows != 1 {
		return errors.Join(rowsErr, errors.New("素材未链接到草稿"))
	}
	return nil
}

func applyDecisionCreated(ctx context.Context, state *applyState, event contracts.Event) error {
	decisionID := stringFrom(event.Payload["decision_id"], "")
	if decisionID == "" {
		return errors.New("DecisionCreated 缺少 decision_id")
	}
	scope := stringFrom(event.Payload["scope_type"], "draft")
	blocking := boolFrom(event.Payload["blocking"], scope == "draft")
	_, err := state.tx.ExecContext(ctx, `
		INSERT INTO decisions(
			decision_id, scope_type, draft_id, type, question, options_json, allow_free_text, status,
			pending_tool_call_json, pending_tool_call_status, blocking, created_by_tool_call_id
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		decisionID, scope, nullableText(event.DraftID), stringFrom(event.Payload["type"], "generic"),
		stringFrom(event.Payload["question"], ""), mustJSON(sliceValue(event.Payload["options"])),
		boolInt(boolFrom(event.Payload["allow_free_text"], true)), stringFrom(event.Payload["status"], "pending"), nullableJSON(event.Payload["pending_tool_call"]),
		nullableString(event.Payload["pending_tool_call_status"]), boolInt(blocking),
		nullableString(event.Payload["created_by_tool_call_id"]),
	)
	if err != nil || scope != "draft" || !blocking {
		return err
	}
	if err := state.touch(ctx, event.DraftID); err != nil {
		return err
	}
	_, err = state.tx.ExecContext(ctx, "UPDATE drafts SET pending_decision_id=? WHERE draft_id=?", decisionID, event.DraftID)
	return err
}

func applyDecisionAnswered(ctx context.Context, state *applyState, event contracts.Event) error {
	decisionID := stringFrom(event.Payload["decision_id"], "")
	answer := event.Payload["answer"]
	if answer == nil {
		answer = map[string]any{
			"option_id": event.Payload["option_id"], "free_text": event.Payload["free_text"],
			"answered_via": stringFrom(event.Payload["answered_via"], "button"),
		}
	}
	result, err := state.tx.ExecContext(ctx, `
		UPDATE decisions SET status='answered', answer_json=?,
		pending_tool_call_status=CASE WHEN pending_tool_call_json IS NULL THEN pending_tool_call_status ELSE 'replayed' END,
		consumed_at=COALESCE(?, consumed_at), replayed_tool_call_id=COALESCE(?, replayed_tool_call_id)
		WHERE decision_id=?`, mustJSON(answer), nullableString(event.Payload["consumed_at"]),
		nullableString(event.Payload["replayed_tool_call_id"]), decisionID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return fmt.Errorf("决策不存在: %s", decisionID)
	}
	if event.DraftID != "" {
		if err := state.touch(ctx, event.DraftID); err != nil {
			return err
		}
		_, err = state.tx.ExecContext(ctx,
			"UPDATE drafts SET pending_decision_id=NULL WHERE draft_id=? AND pending_decision_id=?",
			event.DraftID, decisionID)
	}
	return err
}

func applyConversationContextCleared(ctx context.Context, state *applyState, event contracts.Event) error {
	messageID := stringFrom(event.Payload["message_id"], "")
	if messageID == "" {
		return errors.New("ConversationContextCleared 缺少 message_id")
	}
	if err := state.touch(ctx, event.DraftID); err != nil {
		return err
	}
	if _, err := state.tx.ExecContext(ctx, `
		UPDATE decisions SET status='cancelled',
		pending_tool_call_status=CASE
			WHEN pending_tool_call_json IS NULL OR pending_tool_call_json='null' THEN pending_tool_call_status
			ELSE 'discarded'
		END
		WHERE draft_id=? AND status='pending'`, event.DraftID); err != nil {
		return err
	}
	if _, err := state.tx.ExecContext(ctx,
		"DELETE FROM agent_context_checkpoints WHERE draft_id=?", event.DraftID,
	); err != nil {
		return err
	}
	_, err := state.tx.ExecContext(ctx, `
		UPDATE drafts SET messages_tail_ref=?, pending_decision_id=NULL WHERE draft_id=?`,
		messageID, event.DraftID)
	return err
}

func applyTimelineCreated(ctx context.Context, state *applyState, event contracts.Event) error {
	version := intFrom(event.Payload["timeline_version"], 0)
	if version < 1 {
		return errors.New("TimelineVersionCreated 缺少 timeline_version")
	}
	document := mapValue(event.Payload["document_json"])
	if len(document) == 0 {
		document = mapValue(event.Payload["timeline"])
	}
	if len(document) == 0 {
		document = emptyTimeline(event.DraftID, version)
	}
	// 单时间线模型：在同一 reducer 事务内用新文档替换旧文档。后续任一步
	// 失败都会整体回滚，因此不会出现草稿暂时没有时间线的可见状态。
	if _, err := state.tx.ExecContext(ctx,
		"DELETE FROM timeline_versions WHERE draft_id=?", event.DraftID,
	); err != nil {
		return err
	}
	_, err := state.tx.ExecContext(ctx, `
		INSERT INTO timeline_versions(
			timeline_id, draft_id, version, created_by_patch_id,
			document_json, validation_report_json, created_at
		) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		stringFrom(event.Payload["timeline_id"], fmt.Sprintf("%s:v%d", event.DraftID, version)),
		event.DraftID, version, nullableString(event.Payload["patch_id"]), mustJSON(document),
		nullableJSON(event.Payload["validation_report"]), state.createdAt,
	)
	if err != nil {
		return err
	}
	operations := sliceValue(event.Payload["edit_operations"])
	if len(operations) > 0 {
		batchID := stringFrom(event.Payload["patch_id"], "")
		if batchID == "" {
			return errors.New("TimelineVersionCreated 编辑日志缺少 patch_id")
		}
		origin := stringFrom(event.Payload["edit_origin"], "agent")
		if _, err := state.tx.ExecContext(ctx, `
			INSERT INTO timeline_edit_batches(
				edit_batch_id,draft_id,actor,origin,operations_json,created_at
			) VALUES(?, ?, ?, ?, ?, ?)`,
			batchID, event.DraftID, event.Actor, origin, mustJSON(operations), state.createdAt,
		); err != nil {
			return err
		}
		// 编辑历史只描述最近意图，不承担撤销/版本恢复。按 reducer 提交顺序
		// 严格限制为 20 批，防止模型上下文和本地数据库无限增长。
		if _, err := state.tx.ExecContext(ctx, `
			DELETE FROM timeline_edit_batches
			WHERE draft_id=? AND rowid NOT IN (
				SELECT rowid FROM timeline_edit_batches
				WHERE draft_id=? ORDER BY rowid DESC LIMIT 20
			)`, event.DraftID, event.DraftID); err != nil {
			return err
		}
	}
	if err := state.touch(ctx, event.DraftID); err != nil {
		return err
	}
	_, err = state.tx.ExecContext(ctx,
		"UPDATE drafts SET timeline_current_version=?, timeline_validated=0 WHERE draft_id=?",
		version, event.DraftID)
	return err
}

func applyTimelineValidation(ctx context.Context, state *applyState, event contracts.Event) error {
	version := intFrom(event.Payload["timeline_version"], 0)
	valid := event.Type == "TimelineValidated"
	report := event.Payload["validation_report"]
	if report == nil {
		report = map[string]any{"valid": valid, "checks": []any{}}
	}
	if _, err := state.tx.ExecContext(ctx, `
		UPDATE timeline_versions SET validation_report_json=? WHERE draft_id=? AND version=?`,
		mustJSON(report), event.DraftID, version,
	); err != nil {
		return err
	}
	if err := state.touch(ctx, event.DraftID); err != nil {
		return err
	}
	_, err := state.tx.ExecContext(ctx, `
		UPDATE drafts SET timeline_validated=?
		WHERE draft_id=? AND timeline_current_version=?`, boolInt(valid), event.DraftID, version)
	return err
}

func applyPreviewRendered(ctx context.Context, state *applyState, event contracts.Event) error {
	previewID := stringFrom(event.Payload["artifact_id"], "")
	objectHash := stringFrom(event.Payload["object_hash"], "")
	version := intFrom(event.Payload["timeline_version"], 0)
	if previewID == "" || objectHash == "" || version < 1 {
		return errors.New("PreviewRendered 缺少 artifact_id/object_hash/timeline_version")
	}
	if err := ensureObject(ctx, state, objectHash, int64From(event.Payload["object_size"], 0)); err != nil {
		return err
	}
	_, err := state.tx.ExecContext(ctx, `
		INSERT INTO previews(
			preview_id, draft_id, timeline_version, object_hash, quality_json,
			render_width, render_height, render_fps, expected_duration_sec, created_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		previewID, event.DraftID, version, objectHash, mustJSON(mapValue(event.Payload["quality"])),
		nullableInt64(event.Payload["render_width"]), nullableInt64(event.Payload["render_height"]),
		nullableFloat(event.Payload["render_fps"]), nullableFloat(event.Payload["expected_duration_sec"]),
		state.createdAt,
	)
	if err != nil {
		return err
	}
	if err := state.touch(ctx, event.DraftID); err != nil {
		return err
	}
	_, err = state.tx.ExecContext(ctx, `
		UPDATE drafts SET preview_current_id=?
		WHERE draft_id=? AND timeline_current_version=?`, previewID, event.DraftID, version)
	return err
}

func applyPreviewViewed(ctx context.Context, state *applyState, event contracts.Event) error {
	previewID := stringFrom(event.Payload["preview_id"], "")
	var exists int
	if err := state.tx.QueryRowContext(ctx,
		"SELECT 1 FROM previews WHERE preview_id=? AND draft_id=?", previewID, event.DraftID,
	).Scan(&exists); err != nil {
		return err
	}
	if err := state.touch(ctx, event.DraftID); err != nil {
		return err
	}
	_, err := state.tx.ExecContext(ctx,
		"UPDATE drafts SET last_viewed_preview_id=? WHERE draft_id=?", previewID, event.DraftID)
	return err
}

func applyExportCompleted(ctx context.Context, state *applyState, event contracts.Event) error {
	exportID := stringFrom(event.Payload["artifact_id"], "")
	objectHash := stringFrom(event.Payload["object_hash"], "")
	version := intFrom(event.Payload["timeline_version"], 0)
	if exportID == "" || objectHash == "" || version < 1 {
		return errors.New("ExportCompleted 缺少 artifact_id/object_hash/timeline_version")
	}
	if err := ensureObject(ctx, state, objectHash, int64From(event.Payload["object_size"], 0)); err != nil {
		return err
	}
	_, err := state.tx.ExecContext(ctx, `
		INSERT INTO exports(export_id, draft_id, timeline_version, object_hash, quality_json, created_at)
		VALUES(?, ?, ?, ?, ?, ?)`, exportID, event.DraftID, version, objectHash,
		mustJSON(mapValue(event.Payload["quality"])), state.createdAt)
	if err != nil {
		return err
	}
	if err := state.touch(ctx, event.DraftID); err != nil {
		return err
	}
	_, err = state.tx.ExecContext(ctx, `
		UPDATE drafts SET export_current_id=?
		WHERE draft_id=? AND timeline_current_version=?`, exportID, event.DraftID, version)
	return err
}

func applyJob(ctx context.Context, state *applyState, event contracts.Event) error {
	jobID := stringFrom(event.Payload["job_id"], "")
	if jobID == "" {
		return fmt.Errorf("%s 缺少 job_id", event.Type)
	}
	status := map[string]string{
		"JobEnqueued": "pending", "JobProgress": "running",
		"JobSucceeded": "succeeded", "JobFailed": "failed", "JobCancelled": "cancelled",
	}[event.Type]
	if event.Type != "JobEnqueued" {
		var currentStatus string
		var currentWorkerID, currentStartedAt sql.NullString
		if err := state.tx.QueryRowContext(ctx,
			"SELECT status, worker_id, started_at FROM jobs WHERE job_id=?", jobID,
		).Scan(&currentStatus, &currentWorkerID, &currentStartedAt); err != nil {
			return err
		}
		if currentStatus == "cancelled" && event.Type != "JobCancelled" {
			return ErrJobCancelled
		}
		if event.Type == "JobCancelled" && currentStatus != "pending" && currentStatus != "running" && currentStatus != "cancelled" {
			return ErrJobNotCancellable
		}
		expectedWorkerID := stringFrom(event.Payload["worker_id"], "")
		expectedStartedAt := stringFrom(event.Payload["started_at"], "")
		if expectedWorkerID != "" || expectedStartedAt != "" {
			if !currentWorkerID.Valid || !currentStartedAt.Valid ||
				currentWorkerID.String != expectedWorkerID || currentStartedAt.String != expectedStartedAt {
				return ErrJobClaimLost
			}
		}
	}
	if event.Type == "JobEnqueued" {
		_, err := state.tx.ExecContext(ctx, `
			INSERT INTO jobs(
				job_id, kind, status, draft_id, requested_by_draft_id, asset_id,
				idempotency_key, payload_json, attempts, max_retries, next_run_at,
				priority, progress, created_at
			) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			jobID, stringFrom(event.Payload["kind"], "unknown"), status,
			nullableText(event.DraftID), nullableString(event.Payload["requested_by_draft_id"]),
			nullableString(event.Payload["asset_id"]),
			stringFrom(event.Payload["idempotency_key"], jobID),
			mustJSON(mapValue(event.Payload["job_payload"])), intFrom(event.Payload["attempts"], 0),
			intFrom(event.Payload["max_retries"], 0), stringFrom(event.Payload["next_run_at"], state.createdAt),
			intFrom(event.Payload["priority"], 100), nullableFloat(event.Payload["progress"]), state.createdAt,
		)
		if err != nil {
			return err
		}
	} else {
		finishedAt := any(nil)
		if status == "succeeded" || status == "failed" || status == "cancelled" {
			finishedAt = state.createdAt
		}
		_, err := state.tx.ExecContext(ctx, `
			UPDATE jobs SET status=?, progress=COALESCE(?, progress),
			result_json=COALESCE(?, result_json), error_json=COALESCE(?, error_json),
			finished_at=COALESCE(?, finished_at)
			WHERE job_id=?`, status, nullableFloat(event.Payload["progress"]),
			nullableJSON(event.Payload["result"]), nullableJSON(event.Payload["error"]), finishedAt, jobID)
		if err != nil {
			return err
		}
		if event.Type == "JobFailed" {
			assetID := stringFrom(event.Payload["asset_id"], "")
			if assetID != "" {
				if _, err := state.tx.ExecContext(ctx, `
					UPDATE assets SET ingest_status='failed', usable=0, failure_json=?
					WHERE asset_id=?`, nullableJSON(event.Payload["error"]), assetID); err != nil {
					return err
				}
			}
		}
	}
	draftID := stringFrom(event.Payload["requested_by_draft_id"], event.DraftID)
	if draftID == "" {
		return nil
	}
	return updateRunningJobs(ctx, state, draftID, jobID, status, event)
}

func updateRunningJobs(
	ctx context.Context,
	state *applyState,
	draftID, jobID, status string,
	event contracts.Event,
) error {
	if err := state.touch(ctx, draftID); err != nil {
		return err
	}
	var raw string
	if err := state.tx.QueryRowContext(ctx,
		"SELECT running_jobs_json FROM drafts WHERE draft_id=?", draftID,
	).Scan(&raw); err != nil {
		return err
	}
	var jobs []map[string]any
	_ = json.Unmarshal([]byte(raw), &jobs)
	filtered := jobs[:0]
	for _, job := range jobs {
		if stringFrom(job["job_id"], "") != jobID {
			filtered = append(filtered, job)
		}
	}
	if status == "pending" || status == "running" {
		filtered = append(filtered, map[string]any{
			"job_id": jobID, "kind": stringFrom(event.Payload["kind"], "unknown"),
			"status": status, "progress": event.Payload["progress"],
		})
	}
	_, err := state.tx.ExecContext(ctx,
		"UPDATE drafts SET running_jobs_json=? WHERE draft_id=?", mustJSON(filtered), draftID)
	return err
}

func persistResultRows(
	ctx context.Context,
	tx *sql.Tx,
	rows ResultRows,
	defaultCreatedAt string,
) error {
	if rows.Message != nil {
		createdAt := rows.Message.CreatedAt
		if createdAt == "" {
			createdAt = defaultCreatedAt
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO messages(message_id, draft_id, role, kind, content, created_at)
			VALUES(?, ?, ?, ?, ?, ?)`, rows.Message.ID, rows.Message.DraftID,
			rows.Message.Role, stringFrom(rows.Message.Kind, "reply"), rows.Message.Content, createdAt,
		); err != nil {
			return err
		}
	}
	for _, summary := range rows.MaterialSummaries {
		createdAt := summary.CreatedAt
		if createdAt == "" {
			createdAt = defaultCreatedAt
		}
		version := summary.Version
		if version < 1 {
			if err := tx.QueryRowContext(ctx, `
				SELECT COALESCE(MAX(version), 0) + 1 FROM material_summaries WHERE asset_id=?`,
				summary.AssetID).Scan(&version); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO material_summaries(
				summary_id, asset_id, version, focus, status, summary_json, model,
				fingerprint, prompt_version, created_at
			) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			summary.ID, summary.AssetID, version, summary.Focus, summary.Status,
			mustJSON(summary.Summary), summary.Model, summary.Fingerprint, summary.PromptVersion, createdAt,
		); err != nil {
			return err
		}
	}
	for _, transcript := range rows.Transcripts {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO transcripts(
				transcript_id, asset_id, provider_id, raw_preserved, utterances_json, vad_segments_json
			) VALUES(?, ?, ?, ?, ?, ?)`, transcript.ID, transcript.AssetID,
			transcript.ProviderID, boolInt(transcript.RawPreserved),
			mustJSON(transcript.Utterances), mustJSON(transcript.VADSegments),
		); err != nil {
			return err
		}
	}
	if checkpoint := rows.AgentContextCheckpoint; checkpoint != nil {
		if checkpoint.DraftID == "" || checkpoint.WindowID == "" ||
			checkpoint.WindowNumber < 1 || checkpoint.HistoryVersion < 1 ||
			checkpoint.BaseSnapshotHash == "" || checkpoint.BaseSnapshot == nil {
			return errors.New("agent context checkpoint 字段不完整")
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO agent_context_checkpoints(
				draft_id,window_id,window_number,history_version,
				base_snapshot_json,base_snapshot_hash,summary,
				compacted_through_message_id,created_at,updated_at
			) VALUES(?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(draft_id) DO UPDATE SET
				window_id=excluded.window_id,
				window_number=excluded.window_number,
				history_version=excluded.history_version,
				base_snapshot_json=excluded.base_snapshot_json,
				base_snapshot_hash=excluded.base_snapshot_hash,
				summary=excluded.summary,
				compacted_through_message_id=excluded.compacted_through_message_id,
				updated_at=excluded.updated_at`,
			checkpoint.DraftID, checkpoint.WindowID, checkpoint.WindowNumber,
			checkpoint.HistoryVersion, mustJSON(checkpoint.BaseSnapshot),
			checkpoint.BaseSnapshotHash, checkpoint.Summary,
			checkpoint.CompactedThroughMessageID, defaultCreatedAt, defaultCreatedAt,
		); err != nil {
			return err
		}
	}
	if plan := rows.DraftPlanUpdate; plan != nil {
		if plan.DraftID == "" || plan.ContentPlan == nil {
			return errors.New("草稿创作计划更新字段不完整")
		}
		encoded, err := json.Marshal(plan.ContentPlan)
		if err != nil {
			return fmt.Errorf("草稿创作计划无法编码: %w", err)
		}
		result, err := tx.ExecContext(ctx,
			"UPDATE drafts SET content_plan_json=? WHERE draft_id=?",
			string(encoded), plan.DraftID,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return fmt.Errorf("草稿创作计划更新未找到草稿 %s", plan.DraftID)
		}
	}
	return nil
}

func appendEvents(
	ctx context.Context,
	state *applyState,
	versions map[string]int,
) ([]AppliedEvent, error) {
	applied := make([]AppliedEvent, 0, len(state.eventsToLog))
	for _, event := range state.eventsToLog {
		data, err := event.JSON()
		if err != nil {
			return nil, err
		}
		mergeKey, err := event.MergeKey()
		if err != nil {
			return nil, err
		}
		var key any
		if mergeKey != "" {
			key = mergeKey
		}
		draftID := event.DraftID
		if draftID == "" {
			draftID = stringFrom(event.Payload["requested_by_draft_id"], "")
		}
		var draftValue any
		if draftID != "" {
			draftValue = draftID
		}
		var versionPointer *int
		if version, ok := versions[draftID]; ok {
			versionPointer = intPointer(version)
		} else if draftID != "" {
			if version, err := lookupDraftVersion(ctx, state.tx, draftID); err == nil {
				versionPointer = intPointer(version)
			}
		}
		result, err := state.tx.ExecContext(ctx, `
			INSERT INTO event_log(
				event_type, actor, draft_id, payload_json, merge_key, state_version, created_at
			) VALUES(?, ?, ?, ?, ?, ?, ?)`,
			event.Type, event.Actor, draftValue, data, key, versionPointer, state.createdAt,
		)
		if err != nil {
			return nil, err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return nil, err
		}
		applied = append(applied, AppliedEvent{ID: id, Type: event.Type, StateVersion: versionPointer})
	}
	return applied, nil
}

func ensureObject(ctx context.Context, state *applyState, hash string, size int64) error {
	relPath := filepath.Join("objects", hash)
	if len(hash) == 64 {
		relPath = filepath.Join(hash[:2], hash[2:4], hash)
	}
	_, err := state.tx.ExecContext(ctx, `
		INSERT INTO objects(hash, rel_path, size, created_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(hash) DO NOTHING`, hash, relPath, size, state.createdAt)
	return err
}

func emptyTimeline(draftID string, version int) map[string]any {
	return map[string]any{
		"timeline_id": fmt.Sprintf("%s:v%d", draftID, version),
		"draft_id":    draftID, "version": version, "fps": 30,
		"duration_frames": 1,
		"tracks": []map[string]any{
			{"track_id": "visual_base", "track_type": "primary_visual", "clips": []any{}},
			{"track_id": "visual_overlay", "track_type": "visual_overlay", "clips": []any{}},
			{"track_id": "original_audio", "track_type": "audio", "clips": []any{}},
			{"track_id": "voiceover", "track_type": "audio", "clips": []any{}},
			{"track_id": "bgm", "track_type": "audio", "clips": []any{}},
			{"track_id": "subtitles", "track_type": "text", "clips": []any{}},
		},
	}
}

func emptyResultRows(rows ResultRows) bool {
	return rows.Message == nil && len(rows.MaterialSummaries) == 0 &&
		len(rows.Transcripts) == 0 && rows.AgentContextCheckpoint == nil &&
		rows.DraftPlanUpdate == nil
}

func sortedKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func intPointer(value int) *int { return &value }

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableString(value any) any {
	text, ok := value.(string)
	if !ok || text == "" {
		return nil
	}
	return text
}

func nullableInt64(value any) any {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case *int:
		if typed != nil {
			return int64(*typed)
		}
		return nil
	case *int64:
		if typed != nil {
			return *typed
		}
		return nil
	case float64:
		return int64(typed)
	default:
		return nil
	}
}

func nullableFloat(value any) any {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	default:
		return nil
	}
}

func nullableJSON(value any) any {
	if value == nil {
		return nil
	}
	return mustJSON(value)
}

func mustJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func mapValue(value any) map[string]any {
	if typed, ok := value.(map[string]any); ok && typed != nil {
		return typed
	}
	return map[string]any{}
}

func sliceValue(value any) []any {
	switch typed := value.(type) {
	case []any:
		if typed != nil {
			return typed
		}
	case []map[string]any:
		result := make([]any, len(typed))
		for index := range typed {
			result[index] = typed[index]
		}
		return result
	}
	return []any{}
}

func stringFrom(value any, fallback string) string {
	if typed, ok := value.(string); ok && typed != "" {
		return typed
	}
	return fallback
}

func intFrom(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return fallback
	}
}

func int64From(value any, fallback int64) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	default:
		return fallback
	}
}

func boolFrom(value any, fallback bool) bool {
	if typed, ok := value.(bool); ok {
		return typed
	}
	return fallback
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
