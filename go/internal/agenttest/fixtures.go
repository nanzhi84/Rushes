// Package agenttest 提供跨包共享的测试基建夹具（建库 / 建 draft / 插消息）。
//
// 仅供 _test.go import：本包不在任何生产规则的 depguard allow 列表里,生产代码
// 引用会被 depguard 拒绝(_test.go 已豁免),以此把「测试基建被生产误用」挡在编译期。
package agenttest

import (
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

// AgentTestDatabase 打开一个临时 SQLite 库并在测试结束时关闭。
func AgentTestDatabase(t *testing.T) *storage.DB {
	t.Helper()
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// CreateAgentDraft 通过 reducer 建一个 draft。
func CreateAgentDraft(t *testing.T, database *storage.DB, draftID string) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "DraftCreated", DraftID: draftID, Payload: map[string]any{"name": draftID},
	}}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("create status=%s err=%v", result.Status, err)
	}
}

// InsertAgentMessage 通过 reducer 写一条用户消息。
func InsertAgentMessage(t *testing.T, database *storage.DB, draftID, messageID, content string) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorUser, ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: messageID, DraftID: draftID, Role: "user", Kind: "user", Content: content,
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("message status=%s err=%v", result.Status, err)
	}
}
