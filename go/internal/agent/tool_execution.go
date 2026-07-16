package agent

import (
	"context"
	"errors"
	"fmt"

	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func (service *Service) ExecuteTool(ctx context.Context, name string, input any) (any, error) {
	draftID, err := rushestools.DraftID(ctx)
	if err != nil {
		return nil, err
	}
	release, blockingDecisionID := beginTurnToolCall(ctx)
	defer release()
	if blockingDecisionID != "" {
		return rushestools.ToolResult{
			Status:      "waiting",
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
	case "asset.list_assets":
		return service.toolListAssets(ctx, draftID, input.(rushestools.AssetListInput))
	case "understand.materials":
		return service.toolUnderstand(ctx, draftID, input.(rushestools.UnderstandInput))
	case "media.search_shots":
		return service.toolSearchShots(ctx, draftID, input.(rushestools.ShotSearchInput))
	case "audio.analyze_beats":
		return service.toolAnalyzeAudioBeats(ctx, draftID, input.(rushestools.AudioBeatAnalysisInput))
	case "audio.analyze_speech_pauses":
		return service.toolAnalyzeSpeechPauses(ctx, draftID, input.(rushestools.SpeechPauseAnalysisInput))
	case "speech.inspect":
		return service.toolInspectSpeech(ctx, draftID, input.(rushestools.SpeechInspectInput))
	case "interaction.ask_user":
		return service.toolAskUser(ctx, draftID, input.(rushestools.AskUserInput), nil)
	case "interaction.confirm_action":
		confirmation := input.(rushestools.ConfirmActionInput)
		if err := service.tools.ValidateConfirmation(ctx, confirmation.ToolName, confirmation.Arguments); err != nil {
			return rushestools.ToolResult{
				Status:      "validation_failed",
				Observation: "无法创建确认卡：" + err.Error(),
				Data: map[string]any{
					"error_code": "invalid_confirmation_target",
					"tool_name":  confirmation.ToolName,
					"recovery":   "改用已注册的非 interaction 模型工具，并严格按该工具输入 schema 修正 arguments 后重试。",
				},
			}, nil
		}
		return service.toolAskUser(ctx, draftID, rushestools.AskUserInput{
			Question: confirmation.Question,
			Options: []rushestools.DecisionOptionInput{
				{OptionID: "confirm", Label: "确认"}, {OptionID: "cancel", Label: "取消"},
			},
			AllowFreeText: boolPointer(false),
		}, map[string]any{"tool_name": confirmation.ToolName, "arguments": confirmation.Arguments})
	case "decision.answer":
		return service.toolDecisionAnswer(ctx, draftID, input.(rushestools.DecisionAnswerInput))
	case "plan.update":
		return service.toolPlanUpdate(ctx, draftID, input.(rushestools.PlanUpdateInput))
	case "memory.update":
		return service.toolMemoryUpdate(ctx, draftID, input.(rushestools.MemoryUpdateInput))
	case "timeline.compose_initial":
		return service.toolComposeInitial(ctx, draftID, input.(rushestools.ComposeInitialInput))
	case "timeline.apply_patch":
		return service.toolApplyPatch(ctx, draftID, input.(rushestools.TimelinePatchInput))
	case "timeline.apply_patches":
		return service.toolApplyPatches(ctx, draftID, input.(rushestools.TimelinePatchBatchInput))
	case "timeline.recut_to_beats":
		return service.toolRecutToBeats(ctx, draftID, input.(rushestools.TimelineBeatRecutInput))
	case "timeline.edit_talking_head":
		return service.toolEditTalkingHead(ctx, draftID, input.(rushestools.TalkingHeadEditInput))
	case "timeline.validate":
		return service.toolValidateTimeline(ctx, draftID)
	case "timeline.inspect":
		return service.toolInspectTimeline(ctx, draftID, input.(rushestools.TimelineInspectInput))
	case "render.preview":
		return service.toolEnqueueRender(ctx, draftID, "render_preview", input.(rushestools.RenderPreviewInput).Orientation)
	case "render.final_mp4":
		return service.toolEnqueueRender(ctx, draftID, "render_final", input.(rushestools.RenderFinalInput).Orientation)
	case "render.status":
		return service.toolRenderStatus(ctx, draftID)
	case "render.inspect_preview":
		return service.toolInspectPreview(ctx, draftID, input.(rushestools.RenderInspectInput))
	default:
		return nil, fmt.Errorf("工具未注册执行器: %s", name)
	}
}
