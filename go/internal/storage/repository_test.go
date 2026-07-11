package storage

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestRepositoriesRoundTripAllMaterializedViews(t *testing.T) {
	t.Parallel()
	database, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	now := "2026-07-10T00:00:00Z"
	hash := strings.Repeat("a", 64)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO drafts(draft_id,name,state_version,status,defaults_json,running_jobs_json,brief_json,
			timeline_validated,scratch_memory_json,created_at,updated_at)
		VALUES('draft_active','Active',3,'active','{"fps":30}','[{"job_id":"j"}]','{"goal":"demo"}',1,'{}',?,?),
		      ('draft_trash','Trash',0,'trashed','{}','[]','{}',0,'{}',?,?);
		INSERT INTO objects(hash,rel_path,size,created_at) VALUES(?, 'aa/aa/object', 12, ?);
		INSERT INTO assets(asset_id,storage_mode,object_hash,reference_path,kind,source,filename,hash,mtime,size,
			probe_json,proxy_object_hash,thumbnail_object_hash,ingest_status,understanding_status,usable,failure_json)
		VALUES('asset_1','copy',?,NULL,'video','local','clip.mp4',?,7,12,'{"duration_sec":2}',NULL,NULL,'ready','ready',1,NULL);
		INSERT INTO draft_asset_links(draft_id,asset_id,linked_at,note,rel_dir)
		VALUES('draft_active','asset_1',?,'note','clips');
		INSERT INTO material_summaries(summary_id,asset_id,version,focus,status,summary_json,created_at)
		VALUES('summary_1','asset_1',1,'人物','ready','{"overall":"usable"}',?);
		INSERT INTO messages(message_id,draft_id,role,kind,content,created_at)
		VALUES('msg_1','draft_active','user','user','hello',?),('msg_2','draft_active','assistant','reply','world',?);
		INSERT INTO decisions(decision_id,scope_type,draft_id,type,question,options_json,allow_free_text,status,
			answer_json,pending_tool_call_json,pending_tool_call_status,consumed_at,replayed_tool_call_id,blocking,created_by_tool_call_id)
		VALUES('decision_pending','draft','draft_active','generic','继续？','[{"option_id":"yes","label":"继续"}]',0,'pending',
			NULL,'{"tool_name":"render.preview","arguments":{}}','pending',NULL,NULL,1,'call_1'),
		      ('decision_answered','draft','draft_active','generic','完成？','[]',1,'answered',
			'{"option_id":"yes"}',NULL,NULL,?, 'replay_1',0,NULL);
		INSERT INTO jobs(job_id,kind,status,draft_id,requested_by_draft_id,asset_id,idempotency_key,payload_json,
			attempts,max_retries,next_run_at,priority,progress,created_at)
		VALUES('job_1','ingest','running','draft_active','draft_active','asset_1','job_1','{}',0,2,?,10,0.5,?);
		INSERT INTO event_log(event_type,actor,draft_id,payload_json,merge_key,state_version,created_at)
		VALUES('DraftCreated','user','draft_active','{"event":"DraftCreated","actor":"user","draft_id":"draft_active","payload":{"name":"Active"}}','draft_id=draft_active',3,?),
		      ('AssetImported','user',NULL,'{"event":"AssetImported","actor":"user","payload":{"asset_id":"asset_1","job_id":"import_1"}}','asset_id=asset_1',NULL,?);`,
		now, now, now, now, hash, now, hash, hash, now, now, now, now, now, now, now, now, now, now, now,
	); err != nil {
		t.Fatal(err)
	}

	draft, err := GetDraft(t.Context(), database.Read(), "draft_active")
	if err != nil || draft.Name != "Active" || draft.StateVersion != 3 || !draft.TimelineValidated || len(draft.RunningJobs) != 1 {
		t.Fatalf("draft=%#v err=%v", draft, err)
	}
	if _, err := GetDraft(t.Context(), database.Read(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing draft err=%v", err)
	}
	drafts, err := ListDrafts(t.Context(), database.Read())
	if err != nil || len(drafts) != 1 || drafts[0].ID != "draft_active" {
		t.Fatalf("drafts=%#v err=%v", drafts, err)
	}

	asset, err := GetAsset(t.Context(), database.Read(), "asset_1")
	if err != nil || asset.ObjectHash == nil || asset.MTime == nil || !asset.Usable {
		t.Fatalf("asset=%#v err=%v", asset, err)
	}
	assets, err := ListDraftAssets(t.Context(), database.Read(), "draft_active")
	if err != nil || len(assets) != 1 || assets[0].RelDir == nil {
		t.Fatalf("assets=%#v err=%v", assets, err)
	}
	ids, err := DraftAssetIDs(t.Context(), database.Read(), "draft_active", 10)
	if err != nil || len(ids) != 1 || ids[0] != "asset_1" {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	count, err := DraftMaterialCount(t.Context(), database.Read(), "draft_active")
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	summary, err := LatestMaterialSummary(t.Context(), database.Read(), "asset_1")
	if err != nil || summary["overall"] != "usable" {
		t.Fatalf("summary=%#v err=%v", summary, err)
	}
	if _, err := LatestMaterialSummary(t.Context(), database.Read(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing summary err=%v", err)
	}
	jobs, err := ListAssetJobs(t.Context(), database.Read(), "asset_1")
	if err != nil || len(jobs) != 1 || jobs[0].Progress == nil || *jobs[0].Progress != 0.5 {
		t.Fatalf("jobs=%#v err=%v", jobs, err)
	}
	if _, err := ObjectPathByHash(database.Paths, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nil object err=%v", err)
	}
	if path, err := ObjectPathByHash(database.Paths, &hash); err != nil || !strings.HasSuffix(path, hash) {
		t.Fatalf("object path=%q err=%v", path, err)
	}

	messages, err := ListMessages(t.Context(), database.Read(), "draft_active", 0)
	if err != nil || len(messages) != 2 || messages[0].ID != "msg_1" {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
	pending, err := CurrentDecision(t.Context(), database.Read(), "draft_active")
	if err != nil || pending.ID != "decision_pending" || len(pending.Options) != 1 || pending.AllowFreeText {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	decision, err := GetDecision(t.Context(), database.Read(), "decision_answered")
	if err != nil || decision.Answer["option_id"] != "yes" || decision.ReplayedToolCallID == nil {
		t.Fatalf("decision=%#v err=%v", decision, err)
	}
	pendingRows, err := ListPendingDecisions(t.Context(), database.Read(), "draft_active")
	if err != nil || len(pendingRows) != 1 {
		t.Fatalf("pending rows=%#v err=%v", pendingRows, err)
	}
	if _, err := GetDecision(t.Context(), database.Read(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing decision err=%v", err)
	}

	events, err := ListEventsAfter(t.Context(), database.Read(), 0, nil, 10)
	if err != nil || len(events) != 2 || events[0].DraftID == nil || events[0].StateVersion == nil || events[1].DraftID != nil {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	draftID := "draft_active"
	events, err = ListEventsAfter(t.Context(), database.Read(), 0, &draftID, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("draft events=%#v err=%v", events, err)
	}
}

func TestStorageDecodeFallbacksAndFutureSchemaGuard(t *testing.T) {
	t.Parallel()
	if got := decodeMap("not-json"); len(got) != 0 {
		t.Fatalf("decode map=%#v", got)
	}
	if got := decodeNullMap(sql.NullString{}); got != nil {
		t.Fatalf("decode null=%#v", got)
	}
	if got := decodeMapSlice(""); len(got) != 0 {
		t.Fatalf("decode slice=%#v", got)
	}
	if got := stringPointer(sql.NullString{}); got != nil {
		t.Fatalf("string pointer=%v", got)
	}

	database, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.Write().Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(t.Context()); err == nil {
		t.Fatal("future schema should fail")
	}
}
