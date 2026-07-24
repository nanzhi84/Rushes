package agentexec

import (
	"context"
	"fmt"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

// ProgressFunc 让执行器在不依赖引擎 TurnStreamHub 的前提下上报子过程进度。
// 引擎装配时注入 hub.Record 适配器；事件负载保持 map[string]any 原状。
type ProgressFunc func(draftID string, event map[string]any)

// Executor 承载视频领域的工具执行算法，与编排引擎解耦。依赖经构造注入，
// 回合级回调（draft/reporter/交互态）仍走 context key。引擎侧 Service 保留
// 一个装饰器负责引擎语义（决策屏障、本地导入硬拒绝、确认卡校验），再委托到此。
type Executor struct {
	database         *storage.DB
	analyzer         *understanding.Analyzer
	speechRecognizer contracts.SpeechRecognizer
	progress         ProgressFunc
}

// New 构造领域执行器。progress 可为 nil（非流式场景，如直接 REST 与测试）。
func New(
	database *storage.DB,
	analyzer *understanding.Analyzer,
	speechRecognizer contracts.SpeechRecognizer,
	progress ProgressFunc,
) *Executor {
	return &Executor{
		database:         database,
		analyzer:         analyzer,
		speechRecognizer: speechRecognizer,
		progress:         progress,
	}
}

// recordProgress 在注入了 progress 时上报一条子过程事件，否则静默丢弃。
func (exec *Executor) recordProgress(draftID string, event map[string]any) {
	if exec.progress != nil {
		exec.progress(draftID, event)
	}
}

// ExecuteTool 是领域工具的统一分发入口。引擎语义（beginTurnToolCall 决策屏障、
// asset.import_local_file 硬拒绝、interaction.confirm_action 的 ValidateConfirmation）
// 由引擎侧 Service.ExecuteTool 装饰器在委托到此之前完成，这里只做领域执行。
func (exec *Executor) ExecuteTool(ctx context.Context, name string, input any) (any, error) {
	draftID, err := rushestools.DraftID(ctx)
	if err != nil {
		return nil, err
	}
	switch name {
	case "asset.list_assets":
		return exec.ToolListAssets(ctx, draftID, input.(rushestools.AssetListInput))
	case "media.detect_shots":
		return exec.toolDetectShots(ctx, draftID, input.(rushestools.DetectShotsInput))
	case "shot.search":
		return exec.toolSearchShots(ctx, draftID, input.(rushestools.ShotSearchInput))
	case "audio.analyze_beats":
		return exec.toolAnalyzeAudioBeats(ctx, draftID, input.(rushestools.AudioBeatAnalysisInput))
	case "audio.analyze_speech_pauses":
		return exec.toolAnalyzeSpeechPauses(ctx, draftID, input.(rushestools.SpeechPauseAnalysisInput))
	case "speech.transcribe":
		return exec.toolTranscribeSpeech(ctx, draftID, input.(rushestools.SpeechTranscribeInput))
	case "speech.search":
		return exec.toolSearchSpeech(ctx, draftID, input.(rushestools.SpeechSearchInput))
	case "interaction.ask_user":
		return exec.toolAskUser(ctx, draftID, input.(rushestools.AskUserInput), nil)
	case "interaction.confirm_action":
		confirmation := input.(rushestools.ConfirmActionInput)
		return exec.toolAskUser(ctx, draftID, rushestools.AskUserInput{
			Question: confirmation.Question,
			Options: []rushestools.DecisionOptionInput{
				{OptionID: "confirm", Label: "确认"}, {OptionID: "cancel", Label: "取消"},
			},
			AllowFreeText: BoolPointer(false),
		}, map[string]any{"tool_name": confirmation.ToolName, "arguments": confirmation.Arguments})
	case "decision.answer":
		return exec.ToolDecisionAnswer(ctx, draftID, input.(rushestools.DecisionAnswerInput))
	case "plan.update":
		return exec.toolPlanUpdate(ctx, draftID, input.(rushestools.PlanUpdateInput))
	case "memory.set":
		return exec.toolMemorySet(ctx, draftID, input.(rushestools.MemorySetInput))
	case "memory.remove":
		return exec.toolMemoryRemove(ctx, draftID, input.(rushestools.MemoryRemoveInput))
	case "timeline.compose_initial":
		return exec.toolComposeInitial(ctx, draftID, input.(rushestools.ComposeInitialInput))
	case "timeline.apply_patches":
		return exec.toolApplyPatches(ctx, draftID, input.(rushestools.TimelinePatchBatchInput))
	case "timeline.insert", "timeline.delete", "timeline.update", "timeline.split":
		return exec.toolAtomicTimelineEdit(ctx, draftID, name, input)
	case "timeline.recut_to_beats":
		return exec.toolRecutToBeats(ctx, draftID, input.(rushestools.TimelineBeatRecutInput))
	case "timeline.edit_talking_head":
		return exec.toolEditTalkingHead(ctx, draftID, input.(rushestools.TalkingHeadEditInput))
	case "timeline.check":
		return exec.toolCheckTimeline(ctx, draftID)
	case "timeline.inspect":
		return exec.toolInspectTimeline(ctx, draftID, input.(rushestools.TimelineInspectInput))
	case "render.preview":
		return exec.toolEnqueueRender(ctx, draftID, "render_preview", input.(rushestools.RenderPreviewInput).Orientation, nil)
	case "render.final_mp4":
		return exec.toolEnqueueRender(ctx, draftID, "render_final", input.(rushestools.RenderFinalInput).Orientation, nil)
	case "render.status":
		return exec.toolRenderStatus(ctx, draftID)
	case "render.start":
		return exec.toolStartRender(ctx, draftID, input.(rushestools.RenderStartInput))
	case "job.read":
		return exec.toolReadJob(ctx, draftID, input.(rushestools.JobReadInput))
	case "preview.check":
		return exec.toolCheckPreview(ctx, draftID, input.(rushestools.PreviewCheckInput))
	default:
		return nil, fmt.Errorf("工具未注册执行器: %s", name)
	}
}

// SetSpeechRecognizer 在装配后期注入语音识别器（与 Service.SetSpeechRecognizer 同步）。
func (exec *Executor) SetSpeechRecognizer(recognizer contracts.SpeechRecognizer) {
	exec.speechRecognizer = recognizer
}
