package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// ExecuteTool 是引擎侧的工具执行装饰器，实现 tools.Executor。
//
// 责任分界（PR-C 收口锚点）：
//   - 引擎语义在前：本装饰器只处理与编排引擎强绑定的语义——beginTurnToolCall
//     决策屏障（含本回合工具执行互斥）、asset.import_local_file 硬拒绝、
//     interaction.confirm_action 的 ValidateConfirmation（依赖引擎持有的 tools 注册表）。
//   - 领域执行在后：其余一律委托给 agentexec.Executor.ExecuteTool，由领域包完成
//     真正的工具执行，engine 不再感知具体工具清单。
func (service *Service) ExecuteTool(ctx context.Context, name string, input any) (any, error) {
	draftID, err := rushestools.DraftID(ctx)
	if err != nil {
		return nil, err
	}
	if name == "tool.load" {
		inputValue, ok := input.(rushestools.ToolLoadInput)
		if !ok {
			return nil, fmt.Errorf("tool.load 输入类型异常: %T", input)
		}
		return executeToolLoad(ctx, service.tools, inputValue)
	}
	if elapsed, ok := toolDisclosureSessionFromContext(ctx).takeFirstActionRoundtrip(time.Now()); ok {
		metricToolLoadFirstActionRoundtripMS.Observe(elapsed.Milliseconds())
	}
	// 旧客户端或异常 provider 即使绕过 Registry，也不能重新打开 Agent 的最终
	// 导出或 job 轮询能力。用户导出只走独立 HTTP/UI 服务。
	if name == "render.start" {
		var retiredInput struct {
			Kind string `json:"kind"`
		}
		encoded, _ := json.Marshal(input)
		if json.Unmarshal(encoded, &retiredInput) == nil && retiredInput.Kind == "final" {
			metricLLMFinalExportAttempt.Inc()
		}
		return nil, errors.New("render.start 不属于 Agent 能力；工作预览与 Preview QA 由 Harness 自动完成，最终导出由用户在 UI 触发")
	}
	if name == "job.read" {
		return nil, errors.New("job.read 不属于 Agent 能力；长工具由 harness 在当前 turn 等待终态")
	}
	preparedContext, receiptResult, reused, err := service.prepareTimelineMutationReceipt(ctx, name, input)
	if err != nil {
		return nil, err
	}
	if reused {
		return receiptResult, nil
	}
	ctx = preparedContext
	// 普通只读工具取共享锁，资源隔离 detector/索引读取分层锁，其余副作用工具独占。
	// Effect/Family 分类事实源仍是 registry，索引 footprint 只负责细化执行互斥。
	readOnly := false
	if effect, ok := service.tools.Effect(name); ok {
		readOnly = effect == rushestools.EffectReadOnly
	}
	release, blockingDecisionID := service.beginToolCall(ctx, name, input, readOnly)
	defer release()
	if blockingDecisionID != "" {
		return rushestools.ToolResult{
			Status:      string(rushestools.StatusWaiting),
			Observation: "本回合已经创建阻塞决策卡；必须停止调用工具并等待真实用户回答。",
			Data: map[string]any{
				"decision_id": blockingDecisionID, "blocked_tool": name,
				"turn_should_end": true, "current_turn_unchanged": true,
			},
		}, nil
	}
	switch name {
	case "asset.import_local_file":
		return nil, errors.New("本地导入仅由已确认的 REST 文件选择流程执行")
	case "interaction.confirm_action":
		confirmation := input.(rushestools.ConfirmActionInput)
		if err := service.tools.ValidateConfirmation(ctx, confirmation.ToolName, confirmation.Arguments); err != nil {
			return rushestools.ToolResult{
				Status:      string(rushestools.StatusValidationFailed),
				Observation: "无法创建确认卡：" + err.Error(),
				Data: map[string]any{
					"error_code": string(rushestools.ErrCodeInvalidConfirmationTarget),
					"tool_name":  confirmation.ToolName,
					"recovery":   "改用已注册的非 interaction 模型工具，并严格按该工具输入 schema 修正 arguments 后重试。",
				},
			}, nil
		}
	}
	if toolRequiresTimelineEditLease(name) {
		// REST timeline patches are explicitly marked manual and fenced in the
		// reducer transaction. Agent mutations acquire the lease only after typed
		// input, preconditions, policy, receipt/idempotency and turn admission pass.
		if rushestools.TimelineMutationOrigin(ctx) != "manual" {
			session := timelineEditLeaseSessionFromContext(ctx)
			if session == nil {
				return nil, storage.ErrAgentEditLeaseLost
			}
			if err := session.ensure(ctx); err != nil {
				return nil, err
			}
		}
	}
	rushestools.MarkExecutionStarted(ctx)
	if err := service.prepareOnDemandAudioAnalysis(ctx, draftID, name, input); err != nil {
		return nil, err
	}
	return service.executor.ExecuteTool(ctx, name, input)
}

func (service *Service) beginToolCall(
	ctx context.Context, name string, input any, readOnly bool,
) (func(), string) {
	if encoded, err := json.Marshal(input); err == nil {
		if footprint, _ := indexedResourceFootprint(name, string(encoded)); len(footprint) > 0 {
			accesses := make([]agentexec.IndexedResourceAccess, 0, len(footprint))
			for _, access := range footprint {
				accesses = append(accesses, agentexec.IndexedResourceAccess{
					Domain: access.domain, Resources: access.resources,
					WriteResource: access.writeResource, AllResources: access.allResources,
				})
			}
			return beginIndexedTurnToolCalls(ctx, accesses)
		}
	}
	return beginTurnToolCall(ctx, readOnly)
}
