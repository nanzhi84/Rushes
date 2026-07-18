package agent

import (
	"context"
	"errors"
	"testing"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestDestructiveConfirmationInterceptor(t *testing.T) {
	t.Parallel()
	destructive := rushestools.Spec{Name: "memory.update", Effect: rushestools.EffectDestructive}
	reversible := rushestools.Spec{Name: "plan.update", Effect: rushestools.EffectReversible}
	futureDelete := rushestools.Spec{Name: "asset.delete", Effect: rushestools.EffectDestructive}

	confirmedCtx := agentexec.WithConfirmedToolReplay(context.Background())
	removeInput := rushestools.MemoryUpdateInput{RemoveKeys: []string{"pacing"}}
	addInput := rushestools.MemoryUpdateInput{Entries: []rushestools.MemoryEntryInput{{
		Key: "pacing", Kind: "preference", Statement: "偏快", EvidenceQuote: "偏快",
	}}}

	cases := []struct {
		name    string
		ctx     context.Context
		spec    rushestools.Spec
		input   any
		blocked bool
	}{
		{"可逆工具放行", context.Background(), reversible, rushestools.PlanUpdateInput{}, false},
		{"破坏性删记忆无确认被拦", context.Background(), destructive, removeInput, true},
		{"纯新增豁免（不带 remove_keys）", context.Background(), destructive, addInput, false},
		{"删记忆但持确认凭证放行", confirmedCtx, destructive, removeInput, false},
		{"未来删除类工具默认按破坏性拦", context.Background(), futureDelete, rushestools.MemoryUpdateInput{}, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := destructiveConfirmationInterceptor(test.ctx, test.spec, test.input)
			var rejection *rushestools.InterceptorRejection
			isBlocked := errors.As(err, &rejection)
			if isBlocked != test.blocked {
				t.Fatalf("blocked=%v 期望 %v (err=%v)", isBlocked, test.blocked, err)
			}
			if test.blocked && rejection.Data["error_code"] != "confirmation_required" {
				t.Fatalf("拒绝载荷缺 confirmation_required: %#v", rejection.Data)
			}
		})
	}
}

func TestInterceptorRejectionMiddlewareSkipsRecoveryBudget(t *testing.T) {
	t.Parallel()
	state := newToolRecoveryState()
	ctx := withToolRecoveryState(t.Context(), state)
	calls := 0
	// 用一个「重试安全」的工具名，证明策略拒绝即便对可重试工具也不触发重试。
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(context.Context, *compose.ToolInput) (*compose.ToolOutput, error) {
			calls++
			return nil, &rushestools.InterceptorRejection{
				Observation: "必须先确认",
				Data:        map[string]any{"error_code": "confirmation_required", "tool": "memory.update"},
			}
		},
	)
	output, err := endpoint(ctx, &compose.ToolInput{Name: "asset.list_assets", Arguments: `{}`})
	if err != nil || calls != 1 {
		t.Fatalf("策略拒绝不应重试: calls=%d err=%v", calls, err)
	}
	payload := decodeRecoveryPayload(t, output.Result)
	data := payload["data"].(map[string]any)
	if payload["status"] != "failed" || data["error_code"] != "confirmation_required" {
		t.Fatalf("拒绝未回灌结构化提示: %#v", payload)
	}
	// 关键：不消耗恢复预算——既不记失败链，harness_recovery 也不该出现（未走 decorateToolFailure）。
	if state.unresolved() || data["harness_recovery"] != nil {
		t.Fatalf("策略拒绝不得计入恢复账: unresolved=%v data=%#v", state.unresolved(), data)
	}
}

func TestDestructiveToolBlockedThenExecutesOnConfirmedReplay(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_g2")
	agenttest.InsertAgentMessage(t, database, "draft_g2", "message_g2", "以后都快一点，字幕别遮脸")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	evidenceCtx := rushestools.WithDraftID(
		agentexec.WithMemoryEvidence(t.Context(), storage.UserMemoryEvidenceMessage, "message_g2"),
		"draft_g2",
	)
	// 先种一条长期记忆。
	if _, err := service.ExecuteTool(evidenceCtx, "memory.update", rushestools.MemoryUpdateInput{
		Entries: []rushestools.MemoryEntryInput{{
			Key: "pacing", Kind: "preference", Statement: "成片节奏偏快", EvidenceQuote: "都快一点",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var memoryTool einotool.InvokableTool
	for _, spec := range service.Tools().Specs(true) {
		if spec.Name == "memory.update" {
			memoryTool = spec.Implementation.(einotool.InvokableTool)
		}
	}
	if memoryTool == nil {
		t.Fatal("未找到 memory.update 工具实现")
	}

	// 模型主路径（eino 执行闭包）删记忆但无确认凭证：被拦截器否决，记忆不动。
	_, blockErr := memoryTool.InvokableRun(evidenceCtx, `{"remove_keys":["pacing"]}`)
	var rejection *rushestools.InterceptorRejection
	if !errors.As(blockErr, &rejection) || rejection.Data["error_code"] != "confirmation_required" {
		t.Fatalf("无确认的删记忆应被拦: err=%v", blockErr)
	}
	if memories, err := storage.ListUserMemories(t.Context(), database.Read()); err != nil || len(memories) != 1 {
		t.Fatalf("被拦调用不得改动记忆: memories=%#v err=%v", memories, err)
	}

	// 纯新增路径（无 remove_keys）不受影响：同一 eino 闭包、无确认也能执行。
	if _, err := memoryTool.InvokableRun(evidenceCtx,
		`{"entries":[{"key":"subtitle","kind":"correction","statement":"字幕不要遮脸","evidence_quote":"字幕别遮脸"}]}`,
	); err != nil {
		t.Fatalf("纯新增不应被拦: %v", err)
	}
	if memories, err := storage.ListUserMemories(t.Context(), database.Read()); err != nil || len(memories) != 2 {
		t.Fatalf("纯新增应落库: memories=%#v err=%v", memories, err)
	}

	// 确认后的重放（携带 confirmedToolReplay 凭证，直连 Service.ExecuteTool）执行成功。
	if _, err := service.ExecuteTool(
		agentexec.WithConfirmedToolReplay(evidenceCtx), "memory.update",
		rushestools.MemoryUpdateInput{RemoveKeys: []string{"pacing"}},
	); err != nil {
		t.Fatal(err)
	}
	memories, err := storage.ListUserMemories(t.Context(), database.Read())
	if err != nil || len(memories) != 1 || memories[0].Key != "subtitle" {
		t.Fatalf("确认重放后 pacing 应被删除: memories=%#v err=%v", memories, err)
	}
}
