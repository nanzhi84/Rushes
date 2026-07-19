package reducer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
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

func TestMergeKeyDeduplicatesWithinOneBatch(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	event := contracts.Event{
		Type: "DraftCreated", DraftID: "draft-batch-dedup",
		Payload: map[string]any{"name": "first"},
	}
	result, err := Apply(
		t.Context(), database, []contracts.Event{event, event}, Options{Actor: contracts.ActorUser},
	)
	if err != nil || result.Status != StatusApplied || len(result.AppliedEvents) != 1 || result.SkippedEvents != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	draft, err := storage.GetDraft(t.Context(), database.Read(), event.DraftID)
	if err != nil || draft.Name != "first" {
		t.Fatalf("draft=%#v err=%v", draft, err)
	}
	var eventCount int
	if err := database.Read().QueryRowContext(
		t.Context(), "SELECT COUNT(*) FROM event_log WHERE draft_id=?", event.DraftID,
	).Scan(&eventCount); err != nil || eventCount != 1 {
		t.Fatalf("event count=%d err=%v", eventCount, err)
	}
}

func TestTimelineEditHistoryKeepsOnlyTwentySemanticBatches(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft-edit-history")
	for index := 1; index <= 25; index++ {
		draft, err := storage.GetDraft(t.Context(), database.Read(), "draft-edit-history")
		if err != nil {
			t.Fatal(err)
		}
		baseVersion := draft.StateVersion
		result, err := Apply(t.Context(), database, []contracts.Event{{
			Type: "TimelineVersionCreated", DraftID: "draft-edit-history",
			Payload: map[string]any{
				"timeline_version": index,
				"timeline_id":      fmt.Sprintf("draft-edit-history:v%d", index),
				"patch_id":         fmt.Sprintf("patch_%02d", index),
				"edit_origin":      "manual",
				"edit_operations": []any{map[string]any{
					"kind": "move_clip", "timeline_clip_id": "clip_a", "target_frame": index,
				}},
				"document_json": map[string]any{
					"timeline_id": fmt.Sprintf("draft-edit-history:v%d", index),
					"draft_id":    "draft-edit-history", "version": index,
					"fps": 30, "duration_frames": 1, "tracks": []any{},
				},
			},
		}}, Options{Actor: contracts.ActorUser, BaseVersion: &baseVersion})
		if err != nil || result.Status != StatusApplied {
			t.Fatalf("batch %d result=%#v err=%v", index, result, err)
		}
	}

	batches, err := storage.ListTimelineEditBatches(
		t.Context(), database.Read(), "draft-edit-history", 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 20 || batches[0].ID != "patch_06" || batches[19].ID != "patch_25" {
		t.Fatalf("batches=%#v", batches)
	}
	for _, batch := range batches {
		if batch.Actor != string(contracts.ActorUser) || batch.Origin != "manual" {
			t.Fatalf("人工操作来源错误: %#v", batch)
		}
	}
	var timelineRows, currentVersion int
	if err := database.Read().QueryRow(`
		SELECT COUNT(*), MAX(version) FROM timeline_versions WHERE draft_id='draft-edit-history'`,
	).Scan(&timelineRows, &currentVersion); err != nil {
		t.Fatal(err)
	}
	if timelineRows != 25 || currentVersion != 25 {
		t.Fatalf("timeline rows=%d current=%d", timelineRows, currentVersion)
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

func TestPostPersistFailureRollsBackDraftPlanAndDomainChanges(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft-plan-rollback")
	before, err := storage.GetDraft(t.Context(), database.Read(), "draft-plan-rollback")
	if err != nil {
		t.Fatal(err)
	}
	expectedPlanHash, err := ContentPlanHash(before.ContentPlan)
	if err != nil {
		t.Fatal(err)
	}
	var eventsBefore int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM event_log").Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}

	baseVersion := before.StateVersion
	_, err = Apply(t.Context(), database, []contracts.Event{{
		Type: "DraftRenamed", DraftID: before.ID, BaseVersion: &baseVersion,
		Payload: map[string]any{
			"name": "不应提交的新名称",
			"bad":  make(chan int),
		},
	}}, Options{
		Actor: contracts.ActorAgent,
		ResultRows: ResultRows{DraftPlanUpdate: &DraftPlanUpdateRow{
			DraftID: before.ID, ContentPlan: map[string]any{"should": "rollback"},
			ExpectedPlanHash: expectedPlanHash,
		}},
	})
	if err == nil {
		t.Fatal("appendEvents JSON 编码失败应回滚整个事务")
	}
	after, lookupErr := storage.GetDraft(t.Context(), database.Read(), before.ID)
	if lookupErr != nil {
		t.Fatal(lookupErr)
	}
	if after.Name != before.Name || after.StateVersion != before.StateVersion ||
		after.UpdatedAt != before.UpdatedAt || len(after.ContentPlan) != 0 {
		t.Fatalf("post-persist failure did not roll back: before=%#v after=%#v", before, after)
	}
	var eventsAfter int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM event_log").Scan(&eventsAfter); err != nil {
		t.Fatal(err)
	}
	if eventsAfter != eventsBefore {
		t.Fatalf("event_log count=%d want=%d", eventsAfter, eventsBefore)
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

func TestDraftPlanResultRowCommitsWithoutDomainEventOrVersion(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft-plan")
	before, err := storage.GetDraft(t.Context(), database.Read(), "draft-plan")
	if err != nil {
		t.Fatal(err)
	}
	emptyPlanHash, err := ContentPlanHash(before.ContentPlan)
	if err != nil {
		t.Fatal(err)
	}
	var eventsBefore int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM event_log").Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}

	result, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorAgent,
		ResultRows: ResultRows{DraftPlanUpdate: &DraftPlanUpdateRow{
			DraftID: "draft-plan",
			ContentPlan: map[string]any{
				"story": map[string]any{"pace": "fast"}, "locked": true,
			},
			ExpectedPlanHash: emptyPlanHash,
		}},
	})
	if err != nil || result.Status != StatusApplied || len(result.AppliedEvents) != 0 ||
		len(result.DraftStateVersions) != 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	after, err := storage.GetDraft(t.Context(), database.Read(), "draft-plan")
	if err != nil {
		t.Fatal(err)
	}
	story, _ := after.ContentPlan["story"].(map[string]any)
	if story["pace"] != "fast" || after.ContentPlan["locked"] != true {
		t.Fatalf("content plan=%#v", after.ContentPlan)
	}
	if after.StateVersion != before.StateVersion || after.UpdatedAt != before.UpdatedAt {
		t.Fatalf("plan bookkeeping changed domain version: before=%#v after=%#v", before, after)
	}
	var eventsAfter int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM event_log").Scan(&eventsAfter); err != nil {
		t.Fatal(err)
	}
	if eventsAfter != eventsBefore {
		t.Fatalf("event_log count=%d want=%d", eventsAfter, eventsBefore)
	}

	conflictResult, err := Apply(t.Context(), database, nil, Options{
		Actor: contracts.ActorAgent,
		ResultRows: ResultRows{DraftPlanUpdate: &DraftPlanUpdateRow{
			DraftID: "draft-plan", ContentPlan: map[string]any{"lost": true},
			ExpectedPlanHash: emptyPlanHash,
		}},
	})
	if err != nil || conflictResult.Status != StatusVersionConflict {
		t.Fatalf("conflict result=%#v err=%v", conflictResult, err)
	}
	afterConflict, err := storage.GetDraft(t.Context(), database.Read(), "draft-plan")
	if err != nil || !reflect.DeepEqual(afterConflict, after) {
		t.Fatalf("plan conflict changed draft: before=%#v after=%#v err=%v", after, afterConflict, err)
	}
	currentPlanHash, err := ContentPlanHash(after.ContentPlan)
	if err != nil {
		t.Fatal(err)
	}

	invalidRows := []*DraftPlanUpdateRow{
		{DraftID: "", ContentPlan: map[string]any{}, ExpectedPlanHash: currentPlanHash},
		{DraftID: "draft-plan", ContentPlan: nil, ExpectedPlanHash: currentPlanHash},
		{DraftID: "draft-plan", ContentPlan: map[string]any{}},
		{DraftID: "missing", ContentPlan: map[string]any{}, ExpectedPlanHash: currentPlanHash},
		{DraftID: "draft-plan", ContentPlan: map[string]any{"bad": make(chan int)}, ExpectedPlanHash: currentPlanHash},
	}
	for index, row := range invalidRows {
		if _, err := Apply(t.Context(), database, nil, Options{
			Actor: contracts.ActorAgent, ResultRows: ResultRows{DraftPlanUpdate: row},
		}); err == nil {
			t.Errorf("invalid row %d should fail: %#v", index, row)
		}
	}
	unchanged, err := storage.GetDraft(t.Context(), database.Read(), "draft-plan")
	if err != nil || unchanged.ContentPlan["locked"] != true {
		t.Fatalf("invalid row changed plan=%#v err=%v", unchanged.ContentPlan, err)
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

func TestMaterialUnderstandingRefreshLifecycleKeepsUsableSummary(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	result, err := Apply(t.Context(), database, []contracts.Event{{
		Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset-refresh", "job_id": "job-import-refresh",
			"filename": "refresh.otf", "ingest_status": "ready",
			"understanding_status": "none", "usable": true,
		},
	}}, Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("import result=%#v err=%v", result, err)
	}
	applyMaterial := func(event contracts.Event, rows ResultRows) {
		t.Helper()
		result, err := Apply(t.Context(), database, []contracts.Event{event}, Options{
			Actor: contracts.ActorJob, ResultRows: rows,
		})
		if err != nil || result.Status != StatusApplied {
			t.Fatalf("event=%s payload=%#v result=%#v err=%v", event.Type, event.Payload, result, err)
		}
	}
	assertAsset := func(wantStatus string, wantFailure bool) {
		t.Helper()
		asset, err := storage.GetAsset(t.Context(), database.Read(), "asset-refresh")
		if err != nil {
			t.Fatal(err)
		}
		if asset.UnderstandingStatus != wantStatus || (len(asset.Failure) != 0) != wantFailure {
			t.Fatalf("asset status=%s failure=%#v want status=%s failure=%v",
				asset.UnderstandingStatus, asset.Failure, wantStatus, wantFailure)
		}
	}

	applyMaterial(contracts.Event{Type: "MaterialUnderstandingStarted", Payload: map[string]any{
		"asset_id": "asset-refresh", "job_id": "job-scan-1", "attempt": 0,
	}}, ResultRows{})
	assertAsset("running", false)
	applyMaterial(contracts.Event{Type: "MaterialUnderstandingFailed", Payload: map[string]any{
		"asset_id": "asset-refresh", "job_id": "job-scan-1", "attempt": 0,
		"failure": map[string]any{"message": "首次理解失败"},
	}}, ResultRows{})
	assertAsset("failed", true)

	// 同一 job 的下一 attempt 必须拥有独立 merge key，开始时清掉旧失败；取消后回到 none。
	applyMaterial(contracts.Event{Type: "MaterialUnderstandingStarted", Payload: map[string]any{
		"asset_id": "asset-refresh", "job_id": "job-scan-1", "attempt": 1,
	}}, ResultRows{})
	assertAsset("running", false)
	applyMaterial(contracts.Event{Type: "MaterialUnderstandingFailed", Payload: map[string]any{
		"asset_id": "asset-refresh", "job_id": "job-scan-1", "attempt": 1, "cancelled": true,
		"failure": map[string]any{"message": "用户取消"},
	}}, ResultRows{})
	assertAsset("none", false)

	// Completed 与摘要在同一个 reducer 事务内提交，并清除此前的失败。
	applyMaterial(contracts.Event{Type: "MaterialUnderstandingFailed", Payload: map[string]any{
		"asset_id": "asset-refresh", "job_id": "job-scan-3",
		"failure": map[string]any{"message": "稍后重试"},
	}}, ResultRows{})
	assertAsset("failed", true)
	applyMaterial(contracts.Event{Type: "MaterialUnderstandingCompleted", Payload: map[string]any{
		"asset_id": "asset-refresh", "job_id": "job-scan-4", "summary_id": "summary-refresh-1",
	}}, ResultRows{MaterialSummaries: []MaterialSummaryRow{{
		ID: "summary-refresh-1", AssetID: "asset-refresh", Version: 0,
		Status: "ready", Summary: map[string]any{"overall": "已有可用摘要"},
	}}})
	assertAsset("ready", false)

	// 已有 ready 摘要时，刷新开始或失败都不能把素材降级为不可用状态。
	applyMaterial(contracts.Event{Type: "MaterialUnderstandingStarted", Payload: map[string]any{
		"asset_id": "asset-refresh", "job_id": "job-refresh-1",
	}}, ResultRows{})
	assertAsset("ready", false)
	applyMaterial(contracts.Event{Type: "MaterialUnderstandingFailed", Payload: map[string]any{
		"asset_id": "asset-refresh", "job_id": "job-refresh-1",
		"failure": map[string]any{"message": "刷新失败"},
	}}, ResultRows{})
	assertAsset("ready", false)

	var summaries int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM material_summaries WHERE asset_id='asset-refresh' AND status='ready'`,
	).Scan(&summaries); err != nil || summaries != 1 {
		t.Fatalf("ready summaries=%d err=%v", summaries, err)
	}
}

func TestUnderstandJobStateReconciliationAcrossConcurrentJobs(t *testing.T) {
	t.Parallel()
	const (
		draftID = "draft-understand-concurrency"
		assetID = "asset-understand-concurrency"
	)
	database := openTestDB(t)
	apply := func(events []contracts.Event, rows ResultRows) {
		t.Helper()
		result, err := Apply(t.Context(), database, events, Options{Actor: contracts.ActorJob, ResultRows: rows})
		if err != nil || result.Status != StatusApplied {
			t.Fatalf("events=%#v result=%#v err=%v", events, result, err)
		}
	}
	apply([]contracts.Event{
		{Type: "DraftCreated", DraftID: draftID, Payload: map[string]any{"name": "并发理解"}},
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": assetID, "job_id": "import-" + assetID,
			"filename": "parallel.mov", "kind": "video", "ingest_status": "ready", "usable": true,
		}},
		{Type: "AssetLinked", DraftID: draftID, Payload: map[string]any{"asset_id": assetID}},
	}, ResultRows{})
	assertAsset := func(wantStatus string, wantFailure string) {
		t.Helper()
		asset, err := storage.GetAsset(t.Context(), database.Read(), assetID)
		if err != nil {
			t.Fatal(err)
		}
		if asset.UnderstandingStatus != wantStatus || stringFrom(asset.Failure["message"], "") != wantFailure {
			t.Fatalf("asset status=%s failure=%#v want=%s/%q",
				asset.UnderstandingStatus, asset.Failure, wantStatus, wantFailure)
		}
	}
	enqueue := func(jobID string) {
		t.Helper()
		apply([]contracts.Event{{Type: "JobEnqueued", DraftID: draftID, Payload: map[string]any{
			"job_id": jobID, "kind": "understand", "requested_by_draft_id": draftID,
			"idempotency_key": jobID, "job_payload": map[string]any{"asset_ids": []string{assetID}},
		}}}, ResultRows{})
	}
	terminal := func(eventType, jobID string, payload map[string]any) {
		t.Helper()
		payload["job_id"] = jobID
		payload["kind"] = "understand"
		payload["requested_by_draft_id"] = draftID
		apply([]contracts.Event{{Type: eventType, DraftID: draftID, Payload: payload}}, ResultRows{})
	}

	enqueue("job-understand-a")
	enqueue("job-understand-b")
	assertAsset("running", "")
	terminal("JobProgress", "job-understand-a", map[string]any{"progress": 0.2})
	terminal("JobFailed", "job-understand-a", map[string]any{
		"error": map[string]any{"message": "A 最终失败"},
	})
	// A 的失败不能覆盖仍 pending 的 B。
	assertAsset("running", "")
	terminal("JobCancelled", "job-understand-b", map[string]any{})
	// B 取消后应恢复已有的 A 失败，而不是错误回到 none。
	assertAsset("failed", "A 最终失败")

	enqueue("job-understand-success")
	apply([]contracts.Event{{Type: "MaterialUnderstandingCompleted", Payload: map[string]any{
		"asset_id": assetID, "job_id": "job-understand-success", "attempt": 0,
		"summary_id": "summary-understand-concurrency",
	}}}, ResultRows{MaterialSummaries: []MaterialSummaryRow{{
		ID: "summary-understand-concurrency", AssetID: assetID, Status: "ready",
		Summary: map[string]any{"overall": "并发中的成功摘要"},
	}}})
	terminal("JobSucceeded", "job-understand-success", map[string]any{
		"result": map[string]any{"status": "completed"},
	})
	assertAsset("ready", "")

	enqueue("job-understand-late-failure")
	terminal("JobFailed", "job-understand-late-failure", map[string]any{
		"error": map[string]any{"message": "晚到失败"},
	})
	// ready 摘要必须压过任何晚到失败。
	assertAsset("ready", "")
}

func TestAggregateUnderstandJobReconcilesEveryPayloadAsset(t *testing.T) {
	t.Parallel()
	const draftID = "draft-understand-aggregate-state"
	database := openTestDB(t)
	assetIDs := []string{"asset-aggregate-one", "asset-aggregate-two"}
	events := []contracts.Event{{
		Type: "DraftCreated", DraftID: draftID, Payload: map[string]any{"name": "聚合理解"},
	}}
	for _, assetID := range assetIDs {
		events = append(events,
			contracts.Event{Type: "AssetImported", Payload: map[string]any{
				"asset_id": assetID, "job_id": "import-" + assetID,
				"filename": assetID + ".mov", "kind": "video", "ingest_status": "ready", "usable": true,
			}},
			contracts.Event{Type: "AssetLinked", DraftID: draftID, Payload: map[string]any{"asset_id": assetID}},
		)
	}
	result, err := Apply(t.Context(), database, events, Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("fixture result=%#v err=%v", result, err)
	}
	result, err = Apply(t.Context(), database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: draftID, Payload: map[string]any{
			"job_id": "job-understand-aggregate", "kind": "understand",
			"requested_by_draft_id": draftID, "idempotency_key": "understand-aggregate",
			"job_payload": map[string]any{"asset_ids": []any{assetIDs[0], 42, assetIDs[1]}},
		},
	}}, Options{Actor: contracts.ActorAgent})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("enqueue result=%#v err=%v", result, err)
	}
	var outerAsset sql.NullString
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT asset_id FROM jobs WHERE job_id='job-understand-aggregate'",
	).Scan(&outerAsset); err != nil || outerAsset.Valid {
		t.Fatalf("aggregate outer asset=%#v err=%v", outerAsset, err)
	}
	for _, assetID := range assetIDs {
		asset, err := storage.GetAsset(t.Context(), database.Read(), assetID)
		if err != nil || asset.UnderstandingStatus != "running" {
			t.Fatalf("asset=%s status=%s err=%v", assetID, asset.UnderstandingStatus, err)
		}
	}
	result, err = Apply(t.Context(), database, []contracts.Event{{
		Type: "JobCancelled", DraftID: draftID, Payload: map[string]any{
			"job_id": "job-understand-aggregate", "kind": "understand",
			"requested_by_draft_id": draftID,
		},
	}}, Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("cancel result=%#v err=%v", result, err)
	}
	for _, assetID := range assetIDs {
		asset, err := storage.GetAsset(t.Context(), database.Read(), assetID)
		if err != nil || asset.UnderstandingStatus != "none" || len(asset.Failure) != 0 {
			t.Fatalf("asset=%s status=%s failure=%#v err=%v",
				assetID, asset.UnderstandingStatus, asset.Failure, err)
		}
	}
}

func TestInlineUnderstandingFailureUsesCurrentErrorOverHistoricalJob(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	const assetID = "asset-inline-current-failure"
	result, err := Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": assetID, "job_id": "import-" + assetID,
			"filename": "inline.png", "kind": "image", "ingest_status": "ready", "usable": true,
		}},
		{Type: "JobEnqueued", Payload: map[string]any{
			"job_id": "job-old-understand-failure", "kind": "understand",
			"idempotency_key": "job-old-understand-failure",
			"job_payload":     map[string]any{"asset_ids": []string{assetID}},
		}},
	}, Options{Actor: contracts.ActorAgent})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("setup result=%#v err=%v", result, err)
	}
	result, err = Apply(t.Context(), database, []contracts.Event{{
		Type: "JobFailed", Payload: map[string]any{
			"job_id": "job-old-understand-failure",
			"error":  map[string]any{"message": "旧后台错误"},
		},
	}}, Options{Actor: contracts.ActorJob})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("old failure result=%#v err=%v", result, err)
	}
	result, err = Apply(t.Context(), database, []contracts.Event{
		{Type: "MaterialUnderstandingStarted", Payload: map[string]any{
			"asset_id": assetID, "job_id": "inline-current-run",
		}},
		{Type: "MaterialUnderstandingFailed", Payload: map[string]any{
			"asset_id": assetID, "job_id": "inline-current-run",
			"failure": map[string]any{"message": "本次内联错误"},
		}},
	}, Options{Actor: contracts.ActorAgent})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("inline failure result=%#v err=%v", result, err)
	}
	asset, err := storage.GetAsset(t.Context(), database.Read(), assetID)
	if err != nil || asset.UnderstandingStatus != "failed" ||
		stringFrom(asset.Failure["message"], "") != "本次内联错误" {
		t.Fatalf("asset=%#v err=%v", asset, err)
	}
}

func TestJobFailureOnlyUsesPersistedIngestKindAndAsset(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	assets := []string{"asset-ingest", "asset-decoy", "asset-understand"}
	events := make([]contracts.Event, 0, len(assets)+2)
	for _, assetID := range assets {
		events = append(events, contracts.Event{Type: "AssetImported", Payload: map[string]any{
			"asset_id": assetID, "job_id": "import-" + assetID,
			"filename": assetID + ".otf", "ingest_status": "ready",
			"understanding_status": "ready", "usable": true,
		}})
	}
	events = append(events,
		contracts.Event{Type: "JobEnqueued", Payload: map[string]any{
			"job_id": "job-ingest", "kind": "ingest", "asset_id": "asset-ingest",
			"idempotency_key": "job-ingest",
		}},
		contracts.Event{Type: "JobEnqueued", Payload: map[string]any{
			"job_id": "job-understand", "kind": "understand", "asset_id": "asset-understand",
			"idempotency_key": "job-understand",
		}},
	)
	result, err := Apply(t.Context(), database, events, Options{Actor: contracts.ActorAgent})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("setup result=%#v err=%v", result, err)
	}

	// payload 故意提供错误 kind/asset_id，reducer 必须以 jobs 表中的事实为准。
	result, err = Apply(t.Context(), database, []contracts.Event{
		{Type: "JobFailed", Payload: map[string]any{
			"job_id": "job-understand", "kind": "ingest", "asset_id": "asset-decoy",
			"error": map[string]any{"message": "理解任务失败"},
		}},
		{Type: "JobFailed", Payload: map[string]any{
			"job_id": "job-ingest", "kind": "understand", "asset_id": "asset-decoy",
			"error": map[string]any{"message": "导入任务失败"},
		}},
	}, Options{Actor: contracts.ActorJob})
	if err != nil || result.Status != StatusApplied {
		t.Fatalf("fail result=%#v err=%v", result, err)
	}

	for _, item := range []struct {
		assetID     string
		wantIngest  string
		wantUsable  bool
		wantFailure bool
	}{
		{assetID: "asset-ingest", wantIngest: "failed", wantUsable: false, wantFailure: true},
		{assetID: "asset-decoy", wantIngest: "ready", wantUsable: true, wantFailure: false},
		{assetID: "asset-understand", wantIngest: "ready", wantUsable: true, wantFailure: true},
	} {
		asset, err := storage.GetAsset(t.Context(), database.Read(), item.assetID)
		if err != nil {
			t.Fatal(err)
		}
		if asset.IngestStatus != item.wantIngest || asset.Usable != item.wantUsable ||
			(len(asset.Failure) != 0) != item.wantFailure {
			t.Fatalf("%s ingest=%s usable=%v failure=%#v", item.assetID,
				asset.IngestStatus, asset.Usable, asset.Failure)
		}
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
	peaks := strings.Repeat("f", 64)
	applyEvents(contracts.ActorJob,
		contracts.Event{Type: "AssetProbed", Payload: map[string]any{
			"asset_id": "asset-all", "probe": map[string]any{"duration_sec": 2},
			"thumbnail_object_hash": thumbnail, "thumbnail_object_size": 4,
		}},
		contracts.Event{Type: "ProxyGenerated", Payload: map[string]any{
			"asset_id": "asset-all", "proxy_object_hash": proxy, "proxy_object_size": float64(8),
		}},
		contracts.Event{Type: "PeaksGenerated", Payload: map[string]any{
			"asset_id": "asset-all", "peaks_object_hash": peaks, "peaks_object_size": float64(6),
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
	draft, err := storage.GetDraft(t.Context(), database.Read(), "draft-all")
	if err != nil || draft.ExportCurrentID == nil || *draft.ExportCurrentID != "export-all" ||
		draft.LastViewedPreviewID == nil || *draft.LastViewedPreviewID != "preview-all" {
		t.Fatalf("draft=%#v err=%v", draft, err)
	}
	for table, want := range map[string]int{"timeline_versions": 1, "previews": 1, "exports": 1, "jobs": 2, "objects": 6} {
		var count int
		if err := database.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s count=%d want=%d err=%v", table, count, want, err)
		}
	}
	assetAll, err := storage.GetAsset(t.Context(), database.Read(), "asset-all")
	if err != nil || assetAll.PeaksObjectHash == nil || *assetAll.PeaksObjectHash != peaks {
		t.Fatalf("asset peaks hash=%v err=%v", assetAll.PeaksObjectHash, err)
	}
}

func TestReducerRejectsMalformedDeepPayloads(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft-invalid")
	base := 0
	cases := []contracts.Event{
		{Type: "ProxyGenerated", Payload: map[string]any{"asset_id": "a"}},
		{Type: "PeaksGenerated", Payload: map[string]any{"asset_id": "a"}},
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
	if result, err := Apply(t.Context(), database, nil, Options{Actor: contracts.ActorAgent}); err != nil ||
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
	}}, Options{Actor: contracts.ActorAgent})
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
	}}, Options{Actor: contracts.ActorAgent})
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
		{1, int64(1)}, {int64(2), int64(2)}, {float64(3), int64(3)},
		{func() *int { value := 4; return &value }(), int64(4)},
		{func() *int64 { value := int64(5); return &value }(), int64(5)},
		{(*int)(nil), nil}, {"bad", nil},
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

func TestReducerMissingPreviewRollback(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	createDraft(t, database, "draft-missing-rows")
	event := contracts.Event{Type: "PreviewViewed", DraftID: "draft-missing-rows", Payload: map[string]any{"preview_id": "missing"}}
	if result, err := Apply(t.Context(), database, []contracts.Event{event}, Options{Actor: contracts.ActorUser}); err == nil || result.Status == StatusApplied {
		t.Fatalf("event=%s result=%#v err=%v", event.Type, result, err)
	}
}

func TestAgentJobObservationSuppressionRequiresJobID(t *testing.T) {
	t.Parallel()
	database := openTestDB(t)
	result, err := Apply(t.Context(), database, nil, Options{
		Actor:      contracts.ActorAgent,
		ResultRows: ResultRows{AgentJobObservationSuppressions: []AgentJobObservationSuppressionRow{{}}},
	})
	if err == nil || result.Status == StatusApplied {
		t.Fatalf("空 job_id suppression 应失败: result=%#v err=%v", result, err)
	}
}
