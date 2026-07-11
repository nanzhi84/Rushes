package reducer

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func openTestDB(t *testing.T) *storage.DB {
	t.Helper()
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func createDraft(t *testing.T, database *storage.DB, draftID string) Result {
	t.Helper()
	result, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "DraftCreated", DraftID: draftID,
		Payload: map[string]any{"name": "测试草稿"},
	}}, Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("create result=%#v err=%v", result, err)
	}
	return result
}

func TestDraftCreatedAndMergeDedup(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	event := contracts.Event{
		Type: "DraftCreated", DraftID: "draft-1",
		Payload: map[string]any{"name": "第一条", "defaults": map[string]any{"fps": 30}},
	}
	first, err := Apply(t.Context(), database, []contracts.Event{event}, Options{Actor: contracts.ActorUser})
	if err != nil || first.Status != StatusApplied || len(first.AppliedEvents) != 1 {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, err := Apply(t.Context(), database, []contracts.Event{event}, Options{Actor: contracts.ActorUser})
	if err != nil || second.Status != StatusApplied || second.SkippedEvents != 1 || len(second.AppliedEvents) != 0 {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	draft, err := storage.GetDraft(t.Context(), database.Read(), "draft-1")
	if err != nil || draft.StateVersion != 0 || draft.Name != "第一条" {
		t.Fatalf("draft=%#v err=%v", draft, err)
	}
}

func TestStrictPreflightCASAndConflictRollback(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft-1")
	base := 0
	event := contracts.Event{
		Type: "TimelineVersionCreated", DraftID: "draft-1", BaseVersion: &base,
		Payload: map[string]any{"timeline_version": 1},
	}
	result, err := Apply(t.Context(), database, []contracts.Event{event}, Options{Actor: contracts.ActorAgent})
	if err != nil || result.Status != StatusApplied || result.DraftStateVersions["draft-1"] != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	conflict, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineValidated", DraftID: "draft-1", BaseVersion: &base,
		Payload: map[string]any{"timeline_version": 1},
	}}, Options{Actor: contracts.ActorAgent})
	if err != nil || conflict.Status != StatusVersionConflict || conflict.Conflict.ActualStateVersion != 1 {
		t.Fatalf("conflict=%#v err=%v", conflict, err)
	}
	var count int
	if err := database.Read().QueryRow("SELECT COUNT(*) FROM event_log WHERE event_type='TimelineValidated'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("冲突事件不得写入 event_log")
	}
}

func TestValidationFailureRollsBackEventsAndSideRows(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft-1")
	base := 0
	result, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineVersionCreated", DraftID: "draft-1", BaseVersion: &base,
		Payload: map[string]any{"timeline_version": 1},
	}}, Options{
		Actor: contracts.ActorAgent,
		ResultRows: ResultRows{Message: &MessageRow{
			ID: "message-1", DraftID: "draft-1", Role: "assistant", Kind: "reply", Content: "不会提交",
		}},
		Validate: func(context.Context, *sql.Tx, []string) error {
			return errors.New("时间线不变量失败")
		},
	})
	if err != nil || result.Status != StatusValidationFailed {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	for _, table := range []string{"timeline_versions", "messages"} {
		var count int
		if err := database.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s 不应提交", table)
		}
	}
}

func TestResultRowsCommitWithReducer(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft-1")
	assetEvent := contracts.Event{
		Type: "AssetImported",
		Payload: map[string]any{
			"asset_id": "asset-1", "job_id": "job-import-1", "filename": "clip.mp4",
			"reference_path": "/tmp/clip.mp4", "size": 10,
		},
	}
	result, err := Apply(t.Context(), database, []contracts.Event{assetEvent}, Options{
		Actor: contracts.ActorUser,
		ResultRows: ResultRows{
			Message: &MessageRow{ID: "m1", DraftID: "draft-1", Role: "user", Kind: "reply", Content: "hello"},
			MaterialSummaries: []MaterialSummaryRow{{
				ID: "s1", AssetID: "asset-1", Version: 1, Status: "ready",
				Summary: map[string]any{"overall": "测试"},
			}},
			Transcripts: []TranscriptRow{{
				ID: "t1", AssetID: "asset-1", ProviderID: "degraded", RawPreserved: true,
				Utterances: []map[string]any{}, VADSegments: []map[string]any{},
			}},
		},
	})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	for _, table := range []string{"assets", "messages", "material_summaries", "transcripts"} {
		var count int
		if err := database.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
}

func TestMaterialSummaryRowsAllocateMonotonicVersions(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	result, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset-summary", "job_id": "job-summary", "filename": "font.otf",
		},
	}}, Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("asset result=%#v err=%v", result, err)
	}
	for index, id := range []string{"summary-1", "summary-2"} {
		result, err = Apply(t.Context(), database, nil, Options{
			Actor: contracts.ActorAgent,
			ResultRows: ResultRows{MaterialSummaries: []MaterialSummaryRow{{
				ID: id, AssetID: "asset-summary", Version: 0, Status: "ready",
				Summary: map[string]any{"pass": index + 1},
			}}},
		})
		if err != nil || result.Status != StatusApplied {
			t.Fatalf("summary %d result=%#v err=%v", index, result, err)
		}
	}
	rows, err := database.Read().QueryContext(t.Context(), `
		SELECT version FROM material_summaries WHERE asset_id=? ORDER BY version`, "asset-summary")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	versions := []int{}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if len(versions) != 2 || versions[0] != 1 || versions[1] != 2 {
		t.Fatalf("versions=%v", versions)
	}
}

func TestDraftLifecycleCopyAssetUnlinkAndJobCancellation(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft-source")
	hash := strings.Repeat("f", 64)
	result, err := Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset-copy", "job_id": "job-asset-copy", "filename": "clip.mp4",
		}},
		{Type: "AssetLinked", DraftID: "draft-source", Payload: map[string]any{
			"asset_id": "asset-copy", "note": "keep", "rel_dir": "clips",
		}},
	}, Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("assets result=%#v err=%v", result, err)
	}
	base := 0
	result, err = Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineVersionCreated", DraftID: "draft-source", BaseVersion: &base,
		Payload: map[string]any{
			"timeline_version": 1,
			"document_json": map[string]any{
				"timeline_id": "draft-source:v1", "draft_id": "draft-source", "version": 1,
			},
		},
	}}, Options{Actor: contracts.ActorAgent})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("timeline result=%#v err=%v", result, err)
	}
	result, err = Apply(t.Context(), database, []contracts.Event{{
		Type: "PreviewRendered", DraftID: "draft-source", Payload: map[string]any{
			"artifact_id": "preview-copy", "timeline_version": 1,
			"object_hash": hash, "object_size": 1,
		},
	}, {
		Type: "ExportCompleted", DraftID: "draft-source", Payload: map[string]any{
			"artifact_id": "export-copy", "timeline_version": 1,
			"object_hash": hash, "object_size": 1,
		},
	}}, Options{Actor: contracts.ActorJob})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("preview result=%#v err=%v", result, err)
	}
	result, err = Apply(t.Context(), database, []contracts.Event{
		{Type: "DraftRenamed", DraftID: "draft-source", Payload: map[string]any{"name": "已改名"}},
		{Type: "DraftCopied", DraftID: "draft-copy", Payload: map[string]any{
			"source_draft_id": "draft-source", "name": "副本",
		}},
		{Type: "DraftTrashed", DraftID: "draft-source", Payload: map[string]any{}},
	}, Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("lifecycle result=%#v err=%v", result, err)
	}
	copied, err := storage.GetDraft(t.Context(), database.Read(), "draft-copy")
	if err != nil || copied.Name != "副本" || copied.StateVersion != 0 ||
		copied.PreviewCurrentID == nil || *copied.PreviewCurrentID != "draft-copy:preview-copy" ||
		copied.ExportCurrentID == nil || *copied.ExportCurrentID != "draft-copy:export-copy" {
		t.Fatalf("copied=%#v err=%v", copied, err)
	}
	var timelineDraft, timelineID string
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT draft_id, json_extract(document_json, '$.timeline_id')
		FROM timeline_versions WHERE draft_id='draft-copy' AND version=1`,
	).Scan(&timelineDraft, &timelineID); err != nil || timelineDraft != "draft-copy" || timelineID != "draft-copy:v1" {
		t.Fatalf("timeline draft=%s id=%s err=%v", timelineDraft, timelineID, err)
	}
	result, err = Apply(t.Context(), database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: "draft-copy", Payload: map[string]any{
			"job_id": "job-cancel", "kind": "ingest", "requested_by_draft_id": "draft-copy",
		},
	}}, Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("enqueue result=%#v err=%v", result, err)
	}
	result, err = Apply(t.Context(), database, []contracts.Event{
		{Type: "JobCancelled", DraftID: "draft-copy", Payload: map[string]any{
			"job_id": "job-cancel", "kind": "ingest", "requested_by_draft_id": "draft-copy",
		}},
		{Type: "AssetUnlinked", DraftID: "draft-copy", Payload: map[string]any{"asset_id": "asset-copy"}},
	}, Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("cancel/unlink result=%#v err=%v", result, err)
	}
	var jobStatus string
	var links int
	if err := database.Read().QueryRow("SELECT status FROM jobs WHERE job_id='job-cancel'").Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if err := database.Read().QueryRow("SELECT COUNT(*) FROM draft_asset_links WHERE draft_id='draft-copy'").Scan(&links); err != nil {
		t.Fatal(err)
	}
	source, _ := storage.GetDraft(t.Context(), database.Read(), "draft-source")
	if source.Status != "trashed" || source.Name != "已改名" || jobStatus != "cancelled" || links != 0 {
		t.Fatalf("source=%#v job=%s links=%d", source, jobStatus, links)
	}
	for _, event := range []contracts.Event{
		{Type: "JobProgress", DraftID: "draft-copy", Payload: map[string]any{
			"job_id": "job-cancel", "kind": "ingest", "requested_by_draft_id": "draft-copy", "progress": 0.9,
		}},
		{Type: "JobSucceeded", DraftID: "draft-copy", Payload: map[string]any{
			"job_id": "job-cancel", "kind": "ingest", "requested_by_draft_id": "draft-copy",
		}},
	} {
		result, err = Apply(t.Context(), database, []contracts.Event{event}, Options{Actor: contracts.ActorJob})
		if !errors.Is(err, ErrJobCancelled) || result.Status != "" {
			t.Fatalf("late %s result=%#v err=%v", event.Type, result, err)
		}
	}
	if err := database.Read().QueryRow("SELECT status FROM jobs WHERE job_id='job-cancel'").Scan(&jobStatus); err != nil || jobStatus != "cancelled" {
		t.Fatalf("late terminal changed status=%s err=%v", jobStatus, err)
	}
	result, err = Apply(t.Context(), database, []contracts.Event{
		{Type: "JobEnqueued", DraftID: "draft-copy", Payload: map[string]any{
			"job_id": "job-finished", "kind": "render_preview", "requested_by_draft_id": "draft-copy",
		}},
		{Type: "JobSucceeded", DraftID: "draft-copy", Payload: map[string]any{
			"job_id": "job-finished", "kind": "render_preview", "requested_by_draft_id": "draft-copy",
		}},
	}, Options{Actor: contracts.ActorJob})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("finished job result=%#v err=%v", result, err)
	}
	result, err = Apply(t.Context(), database, []contracts.Event{{
		Type: "JobCancelled", DraftID: "draft-copy", Payload: map[string]any{
			"job_id": "job-finished", "kind": "render_preview", "requested_by_draft_id": "draft-copy",
		},
	}}, Options{Actor: contracts.ActorUser})
	if !errors.Is(err, ErrJobNotCancellable) || result.Status != "" {
		t.Fatalf("finished cancellation result=%#v err=%v", result, err)
	}
	result, err = Apply(t.Context(), database, []contracts.Event{{
		Type: "JobProgress", Payload: map[string]any{"job_id": "missing-job", "progress": 0.5},
	}}, Options{Actor: contracts.ActorJob})
	if err == nil || result.Status != "" {
		t.Fatalf("missing job progress result=%#v err=%v", result, err)
	}
}

func TestDraftCopyRejectsInvalidSourcesAndRollsBack(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft-copy-source")
	createDraft(t, database, "draft-copy-existing")

	for _, item := range []struct {
		name  string
		event contracts.Event
	}{
		{
			name:  "missing source id",
			event: contracts.Event{Type: "DraftCopied", DraftID: "copy-missing-id", Payload: map[string]any{}},
		},
		{
			name: "missing source draft",
			event: contracts.Event{Type: "DraftCopied", DraftID: "copy-missing-source", Payload: map[string]any{
				"source_draft_id": "does-not-exist",
			}},
		},
		{
			name: "target already exists",
			event: contracts.Event{Type: "DraftCopied", DraftID: "draft-copy-existing", Payload: map[string]any{
				"source_draft_id": "draft-copy-source",
			}},
		},
	} {
		t.Run(item.name, func(t *testing.T) {
			result, err := Apply(t.Context(), database, []contracts.Event{item.event}, Options{Actor: contracts.ActorUser})
			if err == nil || result.Status != "" {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}

	base := 0
	result, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineVersionCreated", DraftID: "draft-copy-source", BaseVersion: &base,
		Payload: map[string]any{"timeline_version": 1},
	}}, Options{Actor: contracts.ActorAgent})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("timeline result=%#v err=%v", result, err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		UPDATE timeline_versions SET document_json='{' WHERE draft_id='draft-copy-source'`); err != nil {
		t.Fatal(err)
	}
	result, err = Apply(t.Context(), database, []contracts.Event{{
		Type: "DraftCopied", DraftID: "copy-malformed-timeline", Payload: map[string]any{
			"source_draft_id": "draft-copy-source",
		},
	}}, Options{Actor: contracts.ActorUser})
	if err == nil || result.Status != "" {
		t.Fatalf("malformed timeline result=%#v err=%v", result, err)
	}
	if _, err := storage.GetDraft(t.Context(), database.Read(), "copy-malformed-timeline"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("failed copy must roll back target draft: %v", err)
	}

	result, err = Apply(t.Context(), database, []contracts.Event{{
		Type: "AssetUnlinked", DraftID: "draft-copy-source", Payload: map[string]any{"asset_id": "missing"},
	}}, Options{Actor: contracts.ActorUser})
	if err == nil || result.Status != "" {
		t.Fatalf("missing link result=%#v err=%v", result, err)
	}
}

func TestDraftCopyHelpersPropagateTransactionFailures(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	tx, err := database.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	state := &applyState{
		tx: tx, createdAt: time.Now().UTC().Format(time.RFC3339Nano),
		originalVersions: map[string]int{}, touched: map[string]struct{}{},
	}
	checks := []struct {
		name string
		call func() error
	}{
		{"draft lifecycle", func() error {
			return applyDraftLifecycle(t.Context(), state, contracts.Event{
				Type: "DraftRenamed", DraftID: "draft", Payload: map[string]any{"name": "x"},
			})
		}},
		{"draft copy", func() error {
			return applyDraftCopied(t.Context(), state, contracts.Event{
				Type: "DraftCopied", DraftID: "copy", Payload: map[string]any{"source_draft_id": "source"},
			})
		}},
		{"timelines", func() error { return copyDraftTimelines(t.Context(), state, "source", "target") }},
		{"previews", func() error {
			_, err := copyDraftPreviews(t.Context(), state, "source", "target")
			return err
		}},
		{"exports", func() error {
			_, err := copyDraftExports(t.Context(), state, "source", "target")
			return err
		}},
		{"asset unlink", func() error {
			return applyAssetUnlinked(t.Context(), state, contracts.Event{
				Type: "AssetUnlinked", DraftID: "draft", Payload: map[string]any{"asset_id": "asset"},
			})
		}},
	}
	for _, check := range checks {
		if err := check.call(); err == nil {
			t.Fatalf("%s should propagate transaction failure", check.name)
		}
	}
}

func TestDecisionCreatedPersistsNativeGoOptionSlice(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft-decision")
	base := 0
	result, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "DecisionCreated", DraftID: "draft-decision",
		Payload: map[string]any{
			"decision_id": "decision-confirm", "question": "确认导出？",
			"options": []map[string]any{
				{"option_id": "confirm", "label": "确认"},
				{"option_id": "cancel", "label": "取消"},
			},
		},
	}}, Options{Actor: contracts.ActorAgent, BaseVersion: &base})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	decision, err := storage.GetDecision(t.Context(), database.Read(), "decision-confirm")
	if err != nil {
		t.Fatal(err)
	}
	if len(decision.Options) != 2 || decision.Options[0]["option_id"] != "confirm" {
		t.Fatalf("options=%#v", decision.Options)
	}
}

func TestReducerMaterializesCompleteCoreEventLifecycle(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	now := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	createDraft(t, database, "draft-all")
	hash := strings.Repeat("a", 64)
	applyEvents := func(actor contracts.Actor, events ...contracts.Event) {
		t.Helper()
		result, err := Apply(t.Context(), database, events, Options{Actor: actor, CreatedAt: now})
		if err != nil || result.Status != StatusApplied {
			t.Fatalf("events=%v result=%#v err=%v", eventTypes(events), result, err)
		}
	}
	applyStrict := func(event contracts.Event) {
		t.Helper()
		draft, err := storage.GetDraft(t.Context(), database.Read(), event.DraftID)
		if err != nil {
			t.Fatal(err)
		}
		event.BaseVersion = &draft.StateVersion
		applyEvents(contracts.ActorAgent, event)
	}

	applyEvents(contracts.ActorUser,
		contracts.Event{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset-all", "job_id": "job-import", "storage_mode": "copy",
			"object_hash": hash, "object_size": int64(12), "kind": "video", "filename": "clip.mp4",
			"mtime": float64(7), "size": int64(12), "probe": map[string]any{"duration_sec": 2},
			"understanding_status": "none", "usable": true,
		}},
		contracts.Event{Type: "AssetLinked", DraftID: "draft-all", Payload: map[string]any{
			"asset_id": "asset-all", "note": "主素材", "rel_dir": "clips",
		}},
	)
	thumbnail := strings.Repeat("b", 64)
	proxy := strings.Repeat("c", 64)
	applyEvents(contracts.ActorJob,
		contracts.Event{Type: "AssetProbed", Payload: map[string]any{
			"asset_id": "asset-all", "probe": map[string]any{"duration_sec": 2},
			"thumbnail_object_hash": thumbnail, "thumbnail_object_size": 4,
		}},
		contracts.Event{Type: "ProxyGenerated", Payload: map[string]any{
			"asset_id": "asset-all", "proxy_object_hash": proxy, "proxy_object_size": float64(8),
		}},
		contracts.Event{Type: "MaterialUnderstandingStarted", Payload: map[string]any{"asset_id": "asset-all"}},
		contracts.Event{Type: "MaterialUnderstandingCompleted", Payload: map[string]any{"asset_id": "asset-all"}},
	)
	applyEvents(contracts.ActorAgent,
		contracts.Event{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset-cancel", "job_id": "job-import-cancel", "filename": "cancel.mp4",
		}},
		contracts.Event{Type: "MaterialUnderstandingStarted", Payload: map[string]any{"asset_id": "asset-cancel"}},
		contracts.Event{Type: "MaterialUnderstandingFailed", Payload: map[string]any{
			"asset_id": "asset-cancel", "cancelled": true,
		}},
		contracts.Event{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset-failed", "job_id": "job-import-failed", "filename": "failed.mp4",
		}},
		contracts.Event{Type: "MaterialUnderstandingFailed", Payload: map[string]any{
			"asset_id": "asset-failed", "failure": map[string]any{"message": "vlm failed"},
		}},
	)

	applyStrict(contracts.Event{Type: "DecisionCreated", DraftID: "draft-all", Payload: map[string]any{
		"decision_id": "decision-all", "scope_type": "draft", "question": "继续？",
		"options":           []any{map[string]any{"option_id": "yes", "label": "继续"}},
		"pending_tool_call": map[string]any{"tool_name": "render.preview"}, "blocking": true,
	}})
	applyStrict(contracts.Event{Type: "DecisionAnswered", DraftID: "draft-all", Payload: map[string]any{
		"decision_id": "decision-all", "option_id": "yes", "free_text": "", "answered_via": "button",
		"consumed_at": now.Format(time.RFC3339Nano), "replayed_tool_call_id": "replay-all",
	}})

	applyStrict(contracts.Event{Type: "TimelineVersionCreated", DraftID: "draft-all", Payload: map[string]any{
		"timeline_version": int64(1), "timeline": map[string]any{
			"draft_id": "draft-all", "version": 1, "fps": 30, "duration_frames": 30, "tracks": []any{},
		}, "parent_version": float64(0), "patch_id": "patch-all",
	}})
	applyStrict(contracts.Event{Type: "TimelineValidationFailed", DraftID: "draft-all", Payload: map[string]any{
		"timeline_version": float64(1),
	}})
	applyStrict(contracts.Event{Type: "TimelineValidated", DraftID: "draft-all", Payload: map[string]any{
		"timeline_version": 1, "validation_report": map[string]any{"valid": true},
	}})
	previewHash := strings.Repeat("d", 64)
	applyEvents(contracts.ActorJob, contracts.Event{Type: "PreviewRendered", DraftID: "draft-all", Payload: map[string]any{
		"artifact_id": "preview-all", "timeline_version": 1, "object_hash": previewHash,
		"object_size": 9, "quality": map[string]any{"profile": "preview"},
		"render_width": int64(360), "render_height": float64(640), "render_fps": float32(30),
		"expected_duration_sec": 1,
	}})
	applyEvents(contracts.ActorUser, contracts.Event{Type: "PreviewViewed", DraftID: "draft-all", Payload: map[string]any{
		"preview_id": "preview-all",
	}})
	exportHash := strings.Repeat("e", 64)
	applyEvents(contracts.ActorJob, contracts.Event{Type: "ExportCompleted", DraftID: "draft-all", Payload: map[string]any{
		"artifact_id": "export-all", "timeline_version": 1, "object_hash": exportHash,
		"object_size": 10, "quality": map[string]any{"profile": "final"},
	}})

	applyEvents(contracts.ActorAgent, contracts.Event{Type: "JobEnqueued", DraftID: "draft-all", Payload: map[string]any{
		"job_id": "job-all", "kind": "ingest", "asset_id": "asset-all",
		"requested_by_draft_id": "draft-all", "idempotency_key": "job-all",
		"attempts": int64(0), "max_retries": float64(2), "priority": int64(5), "progress": float32(0),
	}})
	applyEvents(contracts.ActorJob, contracts.Event{Type: "JobProgress", DraftID: "draft-all", Payload: map[string]any{
		"job_id": "job-all", "kind": "ingest", "requested_by_draft_id": "draft-all", "progress": float64(0.5),
	}})
	applyEvents(contracts.ActorJob, contracts.Event{Type: "JobSucceeded", DraftID: "draft-all", Payload: map[string]any{
		"job_id": "job-all", "kind": "ingest", "requested_by_draft_id": "draft-all",
		"progress": 1, "result": map[string]any{"ok": true},
	}})
	applyEvents(contracts.ActorAgent, contracts.Event{Type: "JobEnqueued", DraftID: "draft-all", Payload: map[string]any{
		"job_id": "job-fail", "kind": "ingest", "asset_id": "asset-failed",
		"requested_by_draft_id": "draft-all", "idempotency_key": "job-fail",
	}})
	applyEvents(contracts.ActorJob, contracts.Event{Type: "JobFailed", DraftID: "draft-all", Payload: map[string]any{
		"job_id": "job-fail", "kind": "ingest", "asset_id": "asset-failed",
		"requested_by_draft_id": "draft-all", "error": map[string]any{"message": "failed"},
	}})
	applyStrict(contracts.Event{Type: "TimelineVersionRestored", DraftID: "draft-all", Payload: map[string]any{
		"timeline_version": 1,
	}})

	draft, err := storage.GetDraft(t.Context(), database.Read(), "draft-all")
	if err != nil || draft.ExportCurrentID == nil || *draft.ExportCurrentID != "export-all" ||
		draft.LastViewedPreviewID == nil || *draft.LastViewedPreviewID != "preview-all" {
		t.Fatalf("draft=%#v err=%v", draft, err)
	}
	for table, want := range map[string]int{"previews": 1, "exports": 1, "jobs": 2, "objects": 5} {
		var count int
		if err := database.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s count=%d want=%d err=%v", table, count, want, err)
		}
	}
}

func TestReducerRejectsMalformedDeepPayloads(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft-invalid")
	base := 0
	cases := []contracts.Event{
		{Type: "ProxyGenerated", Payload: map[string]any{"asset_id": "a"}},
		{Type: "TimelineVersionCreated", DraftID: "draft-invalid", BaseVersion: &base, Payload: map[string]any{}},
		{Type: "DecisionAnswered", DraftID: "draft-invalid", BaseVersion: &base, Payload: map[string]any{"decision_id": "missing"}},
		{Type: "PreviewRendered", DraftID: "draft-invalid", Payload: map[string]any{"artifact_id": "p", "timeline_version": 1}},
		{Type: "ExportCompleted", DraftID: "draft-invalid", Payload: map[string]any{"artifact_id": "e", "timeline_version": 1}},
	}
	for _, event := range cases {
		result, err := Apply(t.Context(), database, []contracts.Event{event}, Options{Actor: contracts.ActorAgent})
		if err == nil || result.Status == StatusApplied {
			t.Fatalf("event=%s result=%#v err=%v", event.Type, result, err)
		}
	}
}

func eventTypes(events []contracts.Event) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

func TestReducerWorkspaceDecisionNoDraftJobAndEmptyApplyBranches(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	if result, err := Apply(t.Context(), database, nil, Options{Actor: contracts.ActorSystem}); err != nil ||
		result.Status != StatusApplied || len(result.DraftStateVersions) != 0 {
		t.Fatalf("empty result=%#v err=%v", result, err)
	}
	if _, err := Apply(t.Context(), database, nil, Options{Actor: contracts.Actor("invalid")}); err == nil {
		t.Fatal("invalid actor should fail")
	}
	created, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "DecisionCreated", Payload: map[string]any{
			"decision_id": "workspace-decision", "scope_type": "workspace", "question": "全局？",
			"blocking": false,
		},
	}}, Options{Actor: contracts.ActorSystem})
	if err != nil || created.Status != StatusApplied {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	answered, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "DecisionAnswered", Payload: map[string]any{
			"decision_id": "workspace-decision", "scope_type": "workspace", "answer": map[string]any{"free_text": "ok"},
		},
	}}, Options{Actor: contracts.ActorUser})
	if err != nil || answered.Status != StatusApplied {
		t.Fatalf("answered=%#v err=%v", answered, err)
	}
	job, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "JobEnqueued", Payload: map[string]any{"job_id": "workspace-job", "kind": "noop"},
	}}, Options{Actor: contracts.ActorSystem})
	if err != nil || job.Status != StatusApplied {
		t.Fatalf("job=%#v err=%v", job, err)
	}
}

func TestReducerHelperConversionAndFailureBranches(t *testing.T) {
	t.Parallel()
	if nullableText("") != nil || nullableText("x") == nil || nullableString(1) != nil ||
		nullableString("") != nil || nullableString("x") == nil || nullableJSON(nil) != nil {
		t.Fatal("nullable string/json mismatch")
	}
	for _, item := range []struct {
		value any
		want  any
	}{
		{1, int64(1)}, {int64(2), int64(2)}, {float64(3), int64(3)}, {"bad", nil},
	} {
		if got := nullableInt64(item.value); got != item.want {
			t.Fatalf("nullableInt64(%T)=%v want=%v", item.value, got, item.want)
		}
	}
	for _, item := range []struct {
		value any
		want  any
	}{
		{float64(1), float64(1)}, {float32(2), float64(2)}, {3, float64(3)}, {"bad", nil},
	} {
		if got := nullableFloat(item.value); got != item.want {
			t.Fatalf("nullableFloat(%T)=%v want=%v", item.value, got, item.want)
		}
	}
	if mapValue(nil) == nil || len(mapValue("bad")) != 0 || len(sliceValue([]any{1})) != 1 ||
		len(sliceValue(nil)) != 0 || stringFrom("", "fallback") != "fallback" ||
		intFrom(int64(2), 0) != 2 || intFrom(float64(3), 0) != 3 || intFrom("bad", 4) != 4 ||
		int64From(2, 0) != 2 || int64From(float64(3), 0) != 3 || int64From("bad", 4) != 4 ||
		boolFrom("bad", true) != true || boolInt(false) != 0 || emptyResultRows(ResultRows{}) != true {
		t.Fatal("reducer helper mismatch")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("mustJSON should panic for channel")
		}
	}()
	_ = mustJSON(make(chan int))
}

func TestReducerMissingRestoreAndPreviewRollback(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft-missing-rows")
	base := 0
	for _, event := range []contracts.Event{
		{Type: "TimelineVersionRestored", DraftID: "draft-missing-rows", BaseVersion: &base, Payload: map[string]any{"timeline_version": 9}},
		{Type: "PreviewViewed", DraftID: "draft-missing-rows", Payload: map[string]any{"preview_id": "missing"}},
	} {
		if result, err := Apply(t.Context(), database, []contracts.Event{event}, Options{Actor: contracts.ActorUser}); err == nil || result.Status == StatusApplied {
			t.Fatalf("event=%s result=%#v err=%v", event.Type, result, err)
		}
	}
}
