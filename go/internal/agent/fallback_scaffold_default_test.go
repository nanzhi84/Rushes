//go:build !e2e_scaffold

package agent

import (
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func TestProductionFallbackDoesNotInstallOrRecognizeE2EScaffold(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	const draftID = "draft_production_fallback"
	createAgentDraft(t, database, draftID)
	before, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if service.fallbackScaffold != nil {
		t.Fatalf("生产构建不应安装 fallback scaffold: %#v", service.fallbackScaffold)
	}

	const defaultReply = "未配置模型密钥：已记录你的需求，并保持本地编辑链路可用。"
	for index, marker := range []string{
		"E2E_BLOCK_UNTIL_CANCEL",
		"E2E_CANCEL_UNDERSTANDING",
		"E2E_FULL_MAINLINE",
		"E2E_MEMORY_WRITE",
		"E2E_MEMORY_STATUS",
	} {
		reply, err := service.fallbackTurn(t.Context(), draftID, "message", marker)
		if err != nil || reply != defaultReply {
			t.Fatalf("marker=%s reply=%q err=%v", marker, reply, err)
		}
		if index == 0 && service.Queue().RequestStop(draftID) {
			t.Fatal("直接 fallback 调用不应创建活动 turn")
		}
	}
	after, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil || after.StateVersion != before.StateVersion || after.UpdatedAt != before.UpdatedAt ||
		len(after.ContentPlan) != len(before.ContentPlan) {
		t.Fatalf("E2E 标记不应触发生产副作用: before=%#v after=%#v err=%v", before, after, err)
	}
}
