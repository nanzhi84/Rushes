package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func seedResendMemory(t *testing.T, database *storage.DB, draftID, key, evidenceID, quote string) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{UserMemoryUpserts: []reducer.UserMemoryRow{{
			Key: key, Kind: "preference", Statement: "记忆-" + key,
			EvidenceKind: storage.UserMemoryEvidenceMessage, EvidenceID: evidenceID,
			EvidenceQuote: quote, SourceDraftID: draftID,
		}}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed memory %s result=%#v err=%v", key, result, err)
	}
}

// 重发响应必须带回「被回退对话形成的长期记忆」清单,且幂等重放读回同一清单(落库快照),
// 保证网络重试时前端能一致地重渲染「撤回这些记忆」卡片;记忆本身跨回退存活。
func TestResendResponseCarriesAffectedMemoriesAndReplaysThem(t *testing.T) {
	t.Parallel()
	server, handler, blocking := resendTestServer(t)
	draftID := "draft-resend-memory"
	createDraftThroughAPI(t, handler, draftID)
	insertAPIResendMessage(t, server.database, draftID, "mem-user-1", "第一版内容")
	insertAPIResendMessage(t, server.database, draftID, "mem-user-2", "第二版内容")
	// 证据落在锚点(mem-user-2)上、创建于其后 → 波及;证据在保留的 mem-user-1 上 → 保留。
	seedResendMemory(t, server.database, draftID, "pacing_fast", "mem-user-2", "第二版")
	seedResendMemory(t, server.database, draftID, "kept_pref", "mem-user-1", "第一版")

	resend := httptest.NewRecorder()
	handler.ServeHTTP(resend, apiRequest(t, http.MethodPost, resendPath(draftID, "mem-user-2"),
		map[string]any{"content": "第二版改写", "idempotency_key": "resend-mem"}))
	if resend.Code != http.StatusAccepted {
		t.Fatalf("resend status=%d body=%s", resend.Code, resend.Body.String())
	}
	var response MessageResendResponse
	if err := json.Unmarshal(resend.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.AffectedMemories) != 1 ||
		response.AffectedMemories[0].Key != "pacing_fast" ||
		response.AffectedMemories[0].Statement != "记忆-pacing_fast" {
		t.Fatalf("affected_memories=%#v", response.AffectedMemories)
	}
	select {
	case <-blocking.started:
	case <-time.After(2 * time.Second):
		t.Fatal("resend did not enqueue a new turn")
	}

	// 幂等重放:同 key 同内容读回逐字节相同的响应(含同一波及清单)。
	retry := httptest.NewRecorder()
	handler.ServeHTTP(retry, apiRequest(t, http.MethodPost, resendPath(draftID, "mem-user-2"),
		map[string]any{"content": "第二版改写", "idempotency_key": "resend-mem"}))
	if retry.Code != http.StatusAccepted || retry.Body.String() != resend.Body.String() {
		t.Fatalf("replay status=%d body=%s first=%s", retry.Code, retry.Body.String(), resend.Body.String())
	}
	var replay MessageResendResponse
	if err := json.Unmarshal(retry.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if len(replay.AffectedMemories) != 1 || replay.AffectedMemories[0].Key != "pacing_fast" {
		t.Fatalf("replay affected=%#v", replay.AffectedMemories)
	}

	memories, err := storage.ListUserMemories(t.Context(), server.database.Read())
	if err != nil || len(memories) != 2 {
		t.Fatalf("memories must survive resend: len=%d err=%v", len(memories), err)
	}
}
