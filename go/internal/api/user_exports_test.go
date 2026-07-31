package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agent"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/tools"
)

type forbiddenUserExportModel struct {
	providerCalls atomic.Int64
}

func (modelValue *forbiddenUserExportModel) WithTools(
	[]*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	return modelValue, nil
}

func (modelValue *forbiddenUserExportModel) Generate(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.Message, error) {
	modelValue.providerCalls.Add(1)
	return nil, errors.New("用户最终导出不应调用模型 provider")
}

func (modelValue *forbiddenUserExportModel) Stream(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	modelValue.providerCalls.Add(1)
	return nil, errors.New("用户最终导出不应调用模型 provider")
}

func TestUserExportCreateUsesCurrentExactTimelineAndIsIdempotent(t *testing.T) {
	t.Parallel()
	server, handler, provider := testServerWithUserExportProviderSpy(t)
	const draftID = "draft_user_export_create"
	seedUserExportTimeline(t, server, handler, draftID)

	first := requestUserExport(t, handler, draftID, draftID+":v1", "portrait")
	if first.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", first.Code, first.Body.String())
	}
	var created UserExportRecord
	if err := json.Unmarshal(first.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Status != UserExportRecordStatus("pending") || created.TimelineId != draftID+":v1" ||
		created.TimelineVersion != 1 || created.Orientation != UserExportRecordOrientation("portrait") {
		t.Fatalf("unexpected export: %+v", created)
	}

	duplicate := requestUserExport(t, handler, draftID, draftID+":v1", "portrait")
	var replay UserExportRecord
	if duplicate.Code != http.StatusAccepted || json.Unmarshal(duplicate.Body.Bytes(), &replay) != nil ||
		replay.JobId != created.JobId {
		t.Fatalf("duplicate status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}

	var actor, requestOrigin, payloadTimelineID string
	var payloadVersion, maxRetries int
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT e.actor,
		       json_extract(j.payload_json, '$.request_origin'),
		       json_extract(j.payload_json, '$.timeline_id'),
		       CAST(json_extract(j.payload_json, '$.timeline_version') AS INTEGER),
		       j.max_retries
		FROM jobs j
		JOIN event_log e ON e.event_type='JobEnqueued'
		  AND json_extract(e.payload_json, '$.payload.job_id')=j.job_id
		WHERE j.job_id=?`, created.JobId,
	).Scan(&actor, &requestOrigin, &payloadTimelineID, &payloadVersion, &maxRetries); err != nil {
		t.Fatal(err)
	}
	if actor != string(contracts.ActorUser) || requestOrigin != "user" ||
		payloadTimelineID != draftID+":v1" || payloadVersion != 1 || maxRetries != 0 {
		t.Fatalf(
			"actor=%s origin=%s timeline=%s version=%d max_retries=%d",
			actor, requestOrigin, payloadTimelineID, payloadVersion, maxRetries,
		)
	}

	latest := patchUserExportTimeline(t, handler, draftID)
	stale := requestUserExport(t, handler, draftID, draftID+":v1", "landscape")
	if stale.Code != http.StatusConflict || !containsReason(stale.Body.Bytes(), "stale_timeline") {
		t.Fatalf("stale status=%d body=%s latest=%s", stale.Code, stale.Body.String(), latest)
	}
	if calls := provider.providerCalls.Load(); calls != 0 {
		t.Fatalf("用户最终导出绕过模型失败: provider_calls=%d", calls)
	}
	var jobs int
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM jobs WHERE kind='render_final' AND draft_id=?`, draftID,
	).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("idempotent final jobs=%d err=%v", jobs, err)
	}
}

func TestUserExportExplicitRetryKeepsOriginalTimelineAfterNewEdit(t *testing.T) {
	t.Parallel()
	server, handler, provider := testServerWithUserExportProviderSpy(t)
	const draftID = "draft_user_export_retry"
	seedUserExportTimeline(t, server, handler, draftID)
	createdResponse := requestUserExport(t, handler, draftID, draftID+":v1", "auto")
	var created UserExportRecord
	if createdResponse.Code != http.StatusAccepted || json.Unmarshal(createdResponse.Body.Bytes(), &created) != nil {
		t.Fatalf("create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}

	result, err := reducer.Apply(t.Context(), server.database, []contracts.Event{{
		Type: "JobFailed", DraftID: draftID,
		Payload: map[string]any{
			"job_id": created.JobId, "kind": "render_final",
			"requested_by_draft_id": draftID,
			"error": map[string]any{
				"error_code": "render_failed", "message": "/private/tmp/secret.mp4 failed",
				"retryable": true,
			},
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("fail job status=%s err=%v", result.Status, err)
	}
	patchUserExportTimeline(t, handler, draftID)

	retry := httptest.NewRecorder()
	handler.ServeHTTP(retry, apiRequest(
		t, http.MethodPost,
		"/api/drafts/"+draftID+"/exports/"+created.JobId+"/retry", nil,
	))
	if retry.Code != http.StatusAccepted {
		t.Fatalf("retry status=%d body=%s", retry.Code, retry.Body.String())
	}
	if containsReason(retry.Body.Bytes(), "/private/tmp/secret.mp4") {
		t.Fatalf("retry response leaked local path: %s", retry.Body.String())
	}
	var retried UserExportRecord
	if err := json.Unmarshal(retry.Body.Bytes(), &retried); err != nil {
		t.Fatal(err)
	}
	if retried.JobId == created.JobId || retried.TimelineId != draftID+":v1" ||
		retried.TimelineVersion != 1 || retried.RetryOfJobId == nil || *retried.RetryOfJobId != created.JobId {
		t.Fatalf("retry lost exact target: %+v", retried)
	}

	var version, retryMaxRetries int
	var timelineID, retryOf string
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT CAST(json_extract(payload_json, '$.timeline_version') AS INTEGER),
		       json_extract(payload_json, '$.timeline_id'),
		       json_extract(payload_json, '$.retry_of_job_id'),
		       max_retries
		FROM jobs WHERE job_id=?`, retried.JobId,
	).Scan(&version, &timelineID, &retryOf, &retryMaxRetries); err != nil {
		t.Fatal(err)
	}
	if version != 1 || timelineID != draftID+":v1" || retryOf != created.JobId || retryMaxRetries != 0 {
		t.Fatalf(
			"retry payload version=%d timeline=%s retry_of=%s max_retries=%d",
			version, timelineID, retryOf, retryMaxRetries,
		)
	}
	succeeded, err := reducer.Apply(t.Context(), server.database, []contracts.Event{{
		Type: "JobSucceeded", DraftID: draftID,
		Payload: map[string]any{
			"job_id": retried.JobId, "kind": "render_final", "requested_by_draft_id": draftID,
			"progress": 1.0,
			"result": map[string]any{
				"artifact_id": "export_user_v1", "profile": "portrait",
				"timeline_id": draftID + ":v1", "timeline_version": 1,
			},
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || succeeded.Status != reducer.StatusApplied {
		t.Fatalf("succeed retry status=%s err=%v", succeeded.Status, err)
	}

	listed := httptest.NewRecorder()
	handler.ServeHTTP(listed, apiRequest(t, http.MethodGet, "/api/drafts/"+draftID+"/exports", nil))
	if listed.Code != http.StatusOK || !containsReason(listed.Body.Bytes(), retried.JobId) ||
		!containsReason(listed.Body.Bytes(), created.JobId) {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body.String())
	}
	if containsReason(listed.Body.Bytes(), "/private/tmp/secret.mp4") {
		t.Fatalf("export list leaked local path: %s", listed.Body.String())
	}
	if calls := provider.providerCalls.Load(); calls != 0 {
		t.Fatalf("用户显式重试绕过模型失败: provider_calls=%d", calls)
	}
	var observations, turns, messages int
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM agent_job_observations WHERE job_id IN (?,?)`,
		created.JobId, retried.JobId,
	).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM agent_turn_runs WHERE draft_id=?`, draftID,
	).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM messages WHERE draft_id=?`, draftID,
	).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if observations != 0 || turns != 0 || messages != 0 {
		t.Fatalf(
			"final 导出意外唤醒 Agent: observations=%d turns=%d messages=%d",
			observations, turns, messages,
		)
	}
}

func TestUserExportCreateAndRetryRejectLiveAgentLease(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	const draftID = "draft_user_export_lease"
	seedUserExportTimeline(t, server, handler, draftID)
	createdResponse := requestUserExport(t, handler, draftID, draftID+":v1", "portrait")
	var created UserExportRecord
	if createdResponse.Code != http.StatusAccepted || json.Unmarshal(createdResponse.Body.Bytes(), &created) != nil {
		t.Fatalf("seed export status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	result, err := reducer.Apply(t.Context(), server.database, []contracts.Event{{
		Type: "JobFailed", DraftID: draftID,
		Payload: map[string]any{
			"job_id": created.JobId, "kind": "render_final", "requested_by_draft_id": draftID,
			"error": map[string]any{"error_code": "render_failed", "message": "failed", "retryable": true},
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("fail seed export status=%s err=%v", result.Status, err)
	}
	leaseResult, err := reducer.Apply(t.Context(), server.database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{AgentEditLeaseMutation: &reducer.AgentEditLeaseMutation{
			Operation: reducer.AgentEditLeaseAcquire, DraftID: draftID,
			TurnID: "turn_live", LeaseToken: "token_live", Now: time.Now().UTC(), TTL: time.Minute,
		}},
	})
	if err != nil || leaseResult.Status != reducer.StatusApplied || leaseResult.AgentEditLease == nil ||
		leaseResult.AgentEditLease.Lease == nil {
		t.Fatalf("acquire lease status=%s outcome=%+v err=%v", leaseResult.Status, leaseResult.AgentEditLease, err)
	}

	blocked := requestUserExport(t, handler, draftID, draftID+":v1", "landscape")
	if blocked.Code != http.StatusConflict || !containsReason(blocked.Body.Bytes(), "agent_edit_lease_active") {
		t.Fatalf("lease create status=%d body=%s", blocked.Code, blocked.Body.String())
	}
	blockedRetry := httptest.NewRecorder()
	handler.ServeHTTP(blockedRetry, apiRequest(t, http.MethodPost,
		"/api/drafts/"+draftID+"/exports/"+created.JobId+"/retry", nil))
	if blockedRetry.Code != http.StatusConflict ||
		!containsReason(blockedRetry.Body.Bytes(), "agent_edit_lease_active") {
		t.Fatalf("lease retry status=%d body=%s", blockedRetry.Code, blockedRetry.Body.String())
	}
	var jobs int
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM jobs WHERE kind='render_final' AND draft_id=?`, draftID,
	).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("blocked export jobs=%d err=%v", jobs, err)
	}
	lease := leaseResult.AgentEditLease.Lease
	releaseResult, releaseErr := reducer.Apply(t.Context(), server.database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{AgentEditLeaseMutation: &reducer.AgentEditLeaseMutation{
			Operation: reducer.AgentEditLeaseRelease, DraftID: draftID,
			TurnID: lease.TurnID, LeaseToken: lease.LeaseToken, Now: time.Now().UTC(),
		}},
	})
	if releaseErr != nil || releaseResult.Status != reducer.StatusApplied || releaseResult.AgentEditLease == nil ||
		!releaseResult.AgentEditLease.Released {
		t.Fatalf("release status=%s outcome=%+v err=%v", releaseResult.Status, releaseResult.AgentEditLease, releaseErr)
	}
}

func seedUserExportTimeline(t *testing.T, server *Server, handler http.Handler, draftID string) {
	t.Helper()
	createDraftThroughAPI(t, handler, draftID)
	result, err := reducer.Apply(t.Context(), server.database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset_" + draftID, "job_id": "job_asset_" + draftID,
			"storage_mode": "reference", "reference_path": "/tmp/user-export.mp4",
			"kind": "video", "source": "local_path", "filename": "user-export.mp4",
			"hash": "hash_" + draftID, "size": 1, "ingest_status": "ready",
		}},
		{Type: "AssetLinked", DraftID: draftID, Payload: map[string]any{"asset_id": "asset_" + draftID}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed assets status=%s err=%v", result.Status, err)
	}
	ctx := tools.WithTimelineMutationOrigin(tools.WithDraftID(t.Context(), draftID), "manual")
	if _, err := server.agent.ExecuteTool(ctx, "timeline.insert", tools.TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "asset_" + draftID, "role": "a_roll",
		"source_start_frame": 0, "source_end_frame": 60,
	}); err != nil {
		t.Fatal(err)
	}
}

func patchUserExportTimeline(t *testing.T, handler http.Handler, draftID string) string {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, apiRequest(t, http.MethodPost,
		"/api/drafts/"+draftID+"/timeline/patch", map[string]any{"op": map[string]any{
			"kind": "split_clip", "timeline_clip_id": "clip_v1_001", "split_frame": 30,
		}}))
	if response.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", response.Code, response.Body.String())
	}
	var payload DraftTimelineResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	document, ok := payload.Timeline["timeline_id"].(string)
	if !ok || document == "" {
		t.Fatalf("patch response missing timeline_id: %s", response.Body.String())
	}
	return document
}

func requestUserExport(
	t *testing.T,
	handler http.Handler,
	draftID, timelineID, orientation string,
) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, apiRequest(t, http.MethodPost,
		"/api/drafts/"+draftID+"/exports", map[string]any{
			"timeline_id": timelineID, "orientation": orientation,
		}))
	return response
}

func testServerWithUserExportProviderSpy(
	t *testing.T,
) (*Server, http.Handler, *forbiddenUserExportModel) {
	t.Helper()
	server, handler := testServer(t, t.TempDir(), 0)
	provider := &forbiddenUserExportModel{}
	service, err := agent.NewService(t.Context(), server.database, provider)
	if err != nil {
		t.Fatal(err)
	}
	server.agent.Close()
	server.agent = service
	return server, handler, provider
}

func containsReason(payload []byte, needle string) bool {
	return string(payload) != "" && json.Valid(payload) && containsString(string(payload), needle)
}

func containsString(value, needle string) bool {
	for index := 0; index+len(needle) <= len(value); index++ {
		if value[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}
