package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/providers"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

const (
	realDraftMinimumWorkflows = 5
	realDraftMinimumAttempts  = 100
)

type realDraftFacts struct {
	DraftID          string
	TimelineID       string
	BGMAssetID       string
	BGMClipID        string
	VisualClipID     string
	VisualTrimFrame  int
	DeleteStartFrame int
	DeleteEndFrame   int
	LinkedAssetIDs   map[string]bool
}

type realDraftWorkflow struct {
	Name          string
	ExpectedTool  string
	AllowParallel bool
	Prompt        func(realDraftFacts) string
	ValidateArgs  func(realDraftFacts, string) error
	Validate      func(context.Context, *Service, realDraftFacts, timeline.Document, []string) error
}

type realDraftAttemptFailure struct {
	Workflow string `json:"workflow"`
	Run      int    `json:"run"`
	Tool     string `json:"expected_tool"`
	Error    string `json:"error"`
}

type realDraftToolReport struct {
	GeneratedAt        string                        `json:"generated_at"`
	Model              string                        `json:"model"`
	DraftID            string                        `json:"draft_id"`
	Target             float64                       `json:"target"`
	Attempted          int                           `json:"attempted"`
	Succeeded          int                           `json:"succeeded"`
	Rate               float64                       `json:"rate"`
	ToolCallsAttempted int                           `json:"tool_calls_attempted"`
	ToolCallsSucceeded int                           `json:"tool_calls_succeeded"`
	Workflows          map[string]liveToolEvalMetric `json:"workflows"`
	Failures           []realDraftAttemptFailure     `json:"failures"`
}

// TestRealDraftToolCallingStability 对同一份只读 SQLite 快照派生隔离工作区。
// 每个模型选择都只尝试一次；provider 错误、未调用、错工具、非法 schema、执行失败和
// 后置条件失败均计为失败，不能用隐藏重试抬高成功率。源工作区永远不由 storage.Open 打开。
func TestRealDraftToolCallingStability(t *testing.T) {
	if os.Getenv("RUSHES_REAL_DRAFT_TOOL_EVAL") != "1" {
		t.Skip("设置 RUSHES_REAL_DRAFT_TOOL_EVAL=1 才运行真实草稿工具稳定性评测")
	}
	sourceWorkspace := strings.TrimSpace(os.Getenv("RUSHES_REAL_DRAFT_WORKSPACE"))
	draftID := strings.TrimSpace(os.Getenv("RUSHES_REAL_DRAFT_ID"))
	if sourceWorkspace == "" || draftID == "" {
		t.Fatal("真实草稿评测需要 RUSHES_REAL_DRAFT_WORKSPACE 和 RUSHES_REAL_DRAFT_ID")
	}
	resolvedWorkspace, err := filepath.Abs(sourceWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	sourceDB := filepath.Join(resolvedWorkspace, "rushes.db")
	if info, statErr := os.Stat(sourceDB); statErr != nil || info.IsDir() {
		t.Fatalf("真实草稿源数据库不可用: %v", statErr)
	}

	apiKey := strings.TrimSpace(os.Getenv("RUSHES_DASHSCOPE_API_KEY"))
	if apiKey == "" {
		t.Fatal("真实草稿工具评测缺少 RUSHES_DASHSCOPE_API_KEY")
	}
	modelName := strings.TrimSpace(os.Getenv("RUSHES_QWEN_CHAT_MODEL"))
	if modelName == "" {
		modelName = providers.DefaultChatModel
	}
	tiers, err := providers.NewQwenTiers(t.Context(), providers.QwenTierConfig{
		APIKey: apiKey, BaseURL: os.Getenv("RUSHES_DASHSCOPE_BASE_URL"), ChatModel: modelName,
	})
	if err != nil {
		t.Fatal(err)
	}

	goldenRoot := t.TempDir()
	goldenDB := filepath.Join(goldenRoot, "rushes.db")
	if err := snapshotSQLiteReadOnly(t.Context(), sourceDB, goldenDB); err != nil {
		t.Fatalf("创建真实草稿只读快照: %v", err)
	}
	facts, err := loadRealDraftFacts(t.Context(), goldenDB, draftID)
	if err != nil {
		t.Fatal(err)
	}
	workflows := realDraftWorkflows()
	if len(workflows) < realDraftMinimumWorkflows {
		t.Fatalf("真实草稿 workflow=%d，至少需要 %d", len(workflows), realDraftMinimumWorkflows)
	}
	runs := realDraftEvalRuns(len(workflows))

	report := realDraftToolReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Model:       modelName, DraftID: draftID, Target: liveToolStabilityTarget,
		Workflows: map[string]liveToolEvalMetric{}, Failures: []realDraftAttemptFailure{},
	}
	attemptRoot := t.TempDir()
	for _, workflow := range workflows {
		metric := liveToolEvalMetric{}
		for run := 1; run <= runs; run++ {
			report.Attempted++
			metric.Total++
			attemptWorkspace := filepath.Join(
				attemptRoot, fmt.Sprintf("%s_%03d", workflow.Name, run),
			)
			toolCalls, attemptErr := runRealDraftToolAttempt(
				t.Context(), attemptWorkspace, goldenDB, facts, workflow, tiers.Chat, tiers.Vision,
			)
			report.ToolCallsAttempted += toolCalls
			if removeErr := removeRealDraftAttemptWorkspace(attemptRoot, attemptWorkspace); removeErr != nil {
				attemptErr = errors.Join(attemptErr, removeErr)
			}
			if attemptErr == nil {
				report.Succeeded++
				report.ToolCallsSucceeded += toolCalls
				metric.Succeeded++
				continue
			}
			report.Failures = append(report.Failures, realDraftAttemptFailure{
				Workflow: workflow.Name, Run: run, Tool: workflow.ExpectedTool,
				Error: agentexec.TruncateText(attemptErr.Error(), 1200),
			})
			t.Logf(
				"REAL_DRAFT_TOOL_FAILURE model=%s workflow=%s run=%d error=%s",
				modelName, workflow.Name, run, agentexec.TruncateText(attemptErr.Error(), 1200),
			)
		}
		metric.Rate = ratio(metric.Succeeded, metric.Total)
		report.Workflows[workflow.Name] = metric
		t.Logf(
			"REAL_DRAFT_TOOL_RESULT model=%s workflow=%s succeeded=%d/%d rate=%.2f%%",
			modelName, workflow.Name, metric.Succeeded, metric.Total, metric.Rate*100,
		)
	}
	report.Rate = ratio(report.Succeeded, report.Attempted)
	writeRealDraftToolReport(t, report)
	if err := validateRealDraftToolReport(report); err != nil {
		encoded, _ := json.Marshal(report.Failures)
		t.Fatalf("真实草稿工具稳定性门禁失败: %v failures=%s", err, encoded)
	}
	t.Logf(
		"REAL_DRAFT_TOOL_STABILITY model=%s draft=%s succeeded=%d/%d rate=%.2f%%",
		modelName, draftID, report.Succeeded, report.Attempted, report.Rate*100,
	)
}

func runRealDraftToolAttempt(
	parent context.Context,
	workspace string,
	goldenDB string,
	facts realDraftFacts,
	workflow realDraftWorkflow,
	chat model.ToolCallingChatModel,
	vision model.ToolCallingChatModel,
) (toolCalls int, returnErr error) {
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return 0, err
	}
	if err := copyFile(goldenDB, filepath.Join(workspace, "rushes.db")); err != nil {
		return 0, fmt.Errorf("复制隔离数据库: %w", err)
	}
	database, err := storage.Open(parent, workspace)
	if err != nil {
		return 0, fmt.Errorf("打开隔离数据库: %w", err)
	}
	service, err := NewServiceWithModelsForStartup(parent, database, chat, vision)
	if err != nil {
		_ = database.Close()
		return 0, fmt.Errorf("创建真实 Service: %w", err)
	}
	defer service.Close()
	defer func() { _ = database.Close() }()

	ctx, cancelTurn := context.WithCancelCause(parent)
	turnID := agentexec.RandomID("turn_real_draft")
	sourceMessageID := agentexec.RandomID("message_real_draft")
	ctx = rushestools.WithDraftID(ctx, facts.DraftID)
	ctx = rushestools.WithTurnIdentity(ctx, turnID, sourceMessageID)
	leaseSession := newTimelineEditLeaseSession(
		service.database, facts.DraftID, turnID, cancelTurn,
	)
	ctx = withTimelineEditLeaseSession(ctx, leaseSession)
	ctx = rushestools.WithTimelineWriteAdmission(
		ctx, turnID, leaseSession.token, leaseSession.markLost,
	)
	ctx = withModelToolSurfaceSession(ctx)
	ctx = withToolRecoveryState(ctx, newToolRecoveryState())
	ctx = agentexec.WithTurnInteractionState(
		ctx, agentexec.NewTurnInteractionState(service.indexedResources),
	)
	if err := service.startAgentTurnRun(ctx, turnID, QueueItem{
		DraftID: facts.DraftID, ItemID: sourceMessageID, Kind: QueueUserMessage,
	}); err != nil {
		cancelTurn(err)
		return 0, fmt.Errorf("创建真实 Agent turn: %w", err)
	}
	turnRunStatus := "failed"
	defer func() {
		leaseSession.close()
		if err := service.finishAgentTurnRun(ctx, turnID, turnRunStatus); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("结束真实 Agent turn: %w", err))
		}
		cancelTurn(nil)
	}()

	before, err := timeline.Latest(ctx, database, facts.DraftID)
	if err != nil {
		return 0, fmt.Errorf("读取调用前时间线: %w", err)
	}
	prompt := workflow.Prompt(facts)
	history := []*schema.Message{schema.UserMessage(prompt)}
	selected, err := selectModelToolSurface(ctx, service.tools, history)
	if err != nil {
		return 0, fmt.Errorf("动态工具面选择: %w", err)
	}
	if _, ok := liveWorkflowSpecByName(selected, workflow.ExpectedTool); !ok {
		return 0, fmt.Errorf(
			"动态工具面未绑定期望工具 %s，实际=%s",
			workflow.ExpectedTool, strings.Join(liveWorkflowSpecNames(selected), ","),
		)
	}
	modelToolSurfaceSessionFromContext(ctx).set(liveWorkflowSpecNames(selected))
	// 与正式 dynamicToolSurfaceModel 一致：mutation/preview 能力在 provider
	// 看见之前取得 lease，纯搜索/分析则不提前锁住时间线。
	if specsRequireTimelineEditLease(selected) {
		if err := leaseSession.ensure(ctx); err != nil {
			return 0, fmt.Errorf("取得真实 Agent edit lease: %w", err)
		}
	}
	bound, err := bindLiveWorkflowTools(ctx, chat, selected)
	if err != nil {
		return 0, fmt.Errorf("绑定真实工具面: %w", err)
	}
	snapshot, err := NewContextBuilder(database).Snapshot(ctx, facts.DraftID)
	if err != nil {
		return 0, fmt.Errorf("构造真实 WorldState: %w", err)
	}
	calls, err := liveGenerateWorkflowToolCalls(ctx, bound, snapshot, history)
	if err != nil {
		return 0, fmt.Errorf("模型单次选择: %w", err)
	}
	callCount := len(calls)
	if callCount == 0 {
		return 0, errors.New("模型没有调用工具")
	}
	if !workflow.AllowParallel && callCount != 1 {
		return callCount, fmt.Errorf("模型工具调用数=%d，期望=1", callCount)
	}
	for index := range calls {
		call := &calls[index]
		if call.Function.Name != workflow.ExpectedTool {
			return callCount, fmt.Errorf("模型选择工具=%s，期望=%s", call.Function.Name, workflow.ExpectedTool)
		}
		spec, ok := liveWorkflowSpecByName(selected, call.Function.Name)
		if !ok {
			return callCount, fmt.Errorf("绑定工具面缺少 %s", call.Function.Name)
		}
		if err := validateLiveToolArguments(spec, call.Function.Arguments); err != nil {
			return callCount, fmt.Errorf("模型参数不符合 schema: %w", err)
		}
		if workflow.ValidateArgs != nil {
			if err := workflow.ValidateArgs(facts, call.Function.Arguments); err != nil {
				return callCount, fmt.Errorf("模型参数不符合真实目标: %w", err)
			}
		}
		if strings.TrimSpace(call.ID) == "" {
			return callCount, fmt.Errorf(
				"模型工具调用[%d] %s 缺少 provider tool_call_id",
				index, call.Function.Name,
			)
		}
	}
	// 真实 gate 必须经过与生产 ReAct 相同的 identity/receipt 中间件；关闭
	// 工具内部重试，保持“一次模型选择、一次工具执行”的稳定性计分口径。
	messages, err := invokeLiveWorkflowTools(
		ctx, service, selected, calls,
		newToolRecoveryMiddleware(func(string) bool { return false }, service.tools.ModelReceiptPolicy),
	)
	if err != nil {
		return callCount, fmt.Errorf("实际执行工具: %w", err)
	}
	outputs := make([]string, 0, len(messages))
	for index, message := range messages {
		output := message.Content
		outputs = append(outputs, output)
		if err := validateLiveWorkflowToolOutput(calls[index].Function.Name, output); err != nil {
			return callCount, err
		}
	}
	if workflow.Validate != nil {
		returnErr = workflow.Validate(ctx, service, facts, before, outputs)
		if returnErr != nil {
			arguments := make([]string, 0, len(calls))
			for _, call := range calls {
				arguments = append(arguments, call.Function.Arguments)
			}
			returnErr = fmt.Errorf(
				"%w; arguments=%s outputs=%s",
				returnErr,
				agentexec.TruncateText(strings.Join(arguments, " | "), 600),
				agentexec.TruncateText(strings.Join(outputs, " | "), 600),
			)
		}
	} else {
		for index, output := range outputs {
			if !liveWorkflowToolOutputSucceeded(calls[index].Function.Name, output) {
				returnErr = fmt.Errorf(
					"工具返回未成功: %s", agentexec.TruncateText(output, 600),
				)
				break
			}
		}
	}
	if returnErr == nil {
		turnRunStatus = "finished"
	}
	return callCount, returnErr
}

func realDraftWorkflows() []realDraftWorkflow {
	return []realDraftWorkflow{
		{
			Name: "real_shot_search", ExpectedTool: "shot.search", AllowParallel: true,
			Prompt: func(realDraftFacts) string {
				return "只检索当前真实草稿里适合火焰、舞者、海浪和日落收束的已理解 B-roll；最多返回 12 个客观镜头证据，不要编辑时间线。"
			},
			ValidateArgs: func(_ realDraftFacts, raw string) error {
				arguments, err := decodeRealDraftArguments(raw)
				if err != nil {
					return err
				}
				if strings.TrimSpace(fmt.Sprint(arguments["query"])) == "" {
					return errors.New("shot.search query 为空")
				}
				return nil
			},
			Validate: validateRealDraftShotSearch,
		},
		{
			Name: "real_beat_analysis", ExpectedTool: "audio.analyze_beats",
			Prompt: func(facts realDraftFacts) string {
				return fmt.Sprintf(
					"分析当前卡点混剪的 BGM 素材 %s，返回完整可用拍点证据；只做分析，不编辑时间线。",
					facts.BGMAssetID,
				)
			},
			ValidateArgs: func(facts realDraftFacts, raw string) error {
				return requireRealDraftArguments(raw, map[string]any{"asset_id": facts.BGMAssetID})
			},
			Validate: validateRealDraftBeatAnalysis,
		},
		{
			Name: "real_ripple_delete", ExpectedTool: "timeline.delete",
			Prompt: func(facts realDraftFacts) string {
				return fmt.Sprintf(
					"当前坐标已经确认。只波纹删除时间线 [%d,%d) 这一帧，然后停止；不要合并其它修改。",
					facts.DeleteStartFrame, facts.DeleteEndFrame,
				)
			},
			ValidateArgs: func(facts realDraftFacts, raw string) error {
				return requireRealDraftArguments(raw, map[string]any{
					"kind": "delete_range", "start_frame": facts.DeleteStartFrame,
					"end_frame": facts.DeleteEndFrame,
				})
			},
			Validate: validateRealDraftRippleDelete,
		},
		{
			Name: "real_visual_trim_preserves_audio", ExpectedTool: "timeline.update",
			Prompt: func(facts realDraftFacts) string {
				return fmt.Sprintf(
					"只把主视觉片段 %s 的结尾裁到时间线第 %d 帧；BGM 是独立音频，必须保持原范围。只提交这个原子修改。",
					facts.VisualClipID, facts.VisualTrimFrame,
				)
			},
			ValidateArgs: func(facts realDraftFacts, raw string) error {
				return requireRealDraftArguments(raw, map[string]any{
					"kind": "trim_clip_edge", "timeline_clip_id": facts.VisualClipID,
					"timeline_frame": facts.VisualTrimFrame, "edge": "end",
				})
			},
			Validate: validateRealDraftVisualTrim,
		},
		{
			Name: "real_terminal_truth", ExpectedTool: "timeline.check",
			Prompt: func(facts realDraftFacts) string {
				return fmt.Sprintf(
					"严格检查稳定版本 %s 的结构、内容合同和卡点比例，并如实返回是否通过；不要编辑或渲染。",
					facts.TimelineID,
				)
			},
			ValidateArgs: func(facts realDraftFacts, raw string) error {
				return requireRealDraftArguments(raw, map[string]any{"timeline_id": facts.TimelineID})
			},
			Validate: validateRealDraftTerminalTruth,
		},
	}
}

func validateRealDraftShotSearch(
	_ context.Context,
	_ *Service,
	facts realDraftFacts,
	_ timeline.Document,
	outputs []string,
) error {
	total := 0
	for _, output := range outputs {
		var result rushestools.ShotSearchResult
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			return err
		}
		total += len(result.Shots)
		for _, shot := range result.Shots {
			if !facts.LinkedAssetIDs[shot.AssetID] {
				return fmt.Errorf("镜头 %s 来自草稿外素材 %s", shot.ShotID, shot.AssetID)
			}
		}
	}
	if total == 0 {
		return errors.New("真实素材镜头检索结果为空")
	}
	return nil
}

func validateRealDraftBeatAnalysis(
	_ context.Context,
	_ *Service,
	facts realDraftFacts,
	_ timeline.Document,
	outputs []string,
) error {
	output := outputs[0]
	var result rushestools.AudioBeatAnalysisResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return err
	}
	if result.AssetID != facts.BGMAssetID || result.TimelineFPS <= 0 || len(result.BeatFrames) < 8 {
		return fmt.Errorf(
			"真实 BGM 拍点证据不完整: asset=%s fps=%d beats=%d",
			result.AssetID, result.TimelineFPS, len(result.BeatFrames),
		)
	}
	return nil
}

func validateRealDraftRippleDelete(
	ctx context.Context,
	service *Service,
	_ realDraftFacts,
	before timeline.Document,
	outputs []string,
) error {
	output := outputs[0]
	if !liveWorkflowToolOutputSucceeded("timeline.delete", output) {
		return fmt.Errorf("真实波纹删除未成功: %s", agentexec.TruncateText(output, 600))
	}
	after, err := timeline.Latest(ctx, service.database, before.DraftID)
	if err != nil {
		return err
	}
	if after.Version != before.Version+1 || after.DurationFrames != before.DurationFrames-1 {
		return fmt.Errorf(
			"波纹删除版本/时长不符: before=%s/%d after=%s/%d",
			before.TimelineID, before.DurationFrames, after.TimelineID, after.DurationFrames,
		)
	}
	if report := timeline.Validate(after); !report.Valid {
		return fmt.Errorf("波纹删除后结构无效: %#v", report.Issues)
	}
	return validateRealDraftMutationProof(output, before.TimelineID, after.TimelineID)
}

func validateRealDraftVisualTrim(
	ctx context.Context,
	service *Service,
	facts realDraftFacts,
	before timeline.Document,
	outputs []string,
) error {
	output := outputs[0]
	if !liveWorkflowToolOutputSucceeded("timeline.update", output) {
		return fmt.Errorf("真实视觉裁边未成功: %s", agentexec.TruncateText(output, 600))
	}
	after, err := timeline.Latest(ctx, service.database, before.DraftID)
	if err != nil {
		return err
	}
	if after.Version != before.Version+1 || after.DurationFrames != before.DurationFrames-1 {
		return fmt.Errorf(
			"视觉裁边版本/时长不符: before=%s/%d after=%s/%d",
			before.TimelineID, before.DurationFrames, after.TimelineID, after.DurationFrames,
		)
	}
	trimmed, ok := timelineClipByID(after, facts.VisualClipID)
	if !ok || trimmed.TimelineEndFrame != facts.VisualTrimFrame {
		return fmt.Errorf("视觉裁边目标不符: %#v", trimmed)
	}
	if !reflect.DeepEqual(realDraftIndependentAudio(before), realDraftIndependentAudio(after)) {
		return errors.New("视觉裁边改写了独立 BGM/SFX/voiceover")
	}
	if report := timeline.Validate(after); !report.Valid {
		return fmt.Errorf("视觉裁边后结构无效: %#v", report.Issues)
	}
	return validateRealDraftMutationProof(output, before.TimelineID, after.TimelineID)
}

func validateRealDraftTerminalTruth(
	_ context.Context,
	_ *Service,
	facts realDraftFacts,
	before timeline.Document,
	outputs []string,
) error {
	output := outputs[0]
	var result rushestools.ToolResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return err
	}
	if result.Status != string(rushestools.StatusValidationFailed) {
		return fmt.Errorf("真实失败合同必须如实返回 validation_failed，实际=%s", result.Status)
	}
	if before.TimelineID != facts.TimelineID || fmt.Sprint(result.Data["timeline_id"]) != facts.TimelineID {
		return fmt.Errorf(
			"检查版本未绑定最新真相: latest=%s result=%v",
			before.TimelineID, result.Data["timeline_id"],
		)
	}
	report, ok := result.Data["validation_report"].(map[string]any)
	if !ok {
		return errors.New("timeline.check 缺少 validation_report")
	}
	structural, _ := report["structural_valid"].(bool)
	contract, _ := report["content_contract_valid"].(bool)
	valid, _ := report["valid"].(bool)
	if !structural || contract || valid {
		return fmt.Errorf("真实终态真假不符: validation_report=%#v", report)
	}
	failures, ok := result.Data["contract_failures"].([]any)
	if !ok || len(failures) == 0 {
		return errors.New("真实失败合同没有返回可核验 contract_failures")
	}
	return nil
}

func validateRealDraftMutationProof(output, previousTimelineID, timelineID string) error {
	var result rushestools.ToolResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return err
	}
	if fmt.Sprint(result.Data["previous_timeline_id"]) != previousTimelineID ||
		fmt.Sprint(result.Data["timeline_id"]) != timelineID {
		return fmt.Errorf(
			"编辑结果版本证明不符: previous=%v current=%v",
			result.Data["previous_timeline_id"], result.Data["timeline_id"],
		)
	}
	if _, ok := result.Data["applied_operation"].(map[string]any); !ok {
		return errors.New("编辑结果缺少 applied_operation")
	}
	validation, ok := result.Data["validation_summary"].(map[string]any)
	if !ok {
		return errors.New("编辑结果缺少 validation_summary")
	}
	if structurallyValid, _ := validation["structural_valid"].(bool); !structurallyValid {
		return fmt.Errorf("编辑结果结构校验未通过: %#v", validation)
	}
	return nil
}

func loadRealDraftFacts(ctx context.Context, databasePath, draftID string) (realDraftFacts, error) {
	database, err := openSQLiteReadOnly(databasePath)
	if err != nil {
		return realDraftFacts{}, err
	}
	defer func() { _ = database.Close() }()
	var raw string
	if err := database.QueryRowContext(ctx, `
		SELECT t.document_json
		FROM drafts AS d
		JOIN timeline_versions AS t
			ON t.draft_id=d.draft_id AND t.version=d.timeline_current_version
		WHERE d.draft_id=?`, draftID,
	).Scan(&raw); err != nil {
		return realDraftFacts{}, fmt.Errorf("读取真实草稿最新时间线: %w", err)
	}
	var document timeline.Document
	if err := json.Unmarshal([]byte(raw), &document); err != nil {
		return realDraftFacts{}, err
	}
	facts := realDraftFacts{
		DraftID: draftID, TimelineID: document.TimelineID,
		DeleteStartFrame: 1, DeleteEndFrame: 2, LinkedAssetIDs: map[string]bool{},
	}
	for _, track := range document.Tracks {
		switch track.TrackID {
		case "visual_base":
			if len(track.Clips) > 0 {
				clip := track.Clips[0]
				if clip.TimelineEndFrame-clip.TimelineStartFrame < 3 {
					return realDraftFacts{}, errors.New("真实主视觉首片段过短，无法执行一帧隔离裁边")
				}
				facts.VisualClipID = clip.TimelineClipID
				facts.VisualTrimFrame = clip.TimelineEndFrame - 1
			}
		case "bgm":
			if len(track.Clips) > 0 {
				facts.BGMClipID = track.Clips[0].TimelineClipID
				facts.BGMAssetID = track.Clips[0].AssetID
			}
		}
	}
	if facts.VisualClipID == "" || facts.BGMAssetID == "" || facts.BGMClipID == "" {
		return realDraftFacts{}, errors.New("真实草稿缺少主视觉或 BGM，无法运行五类稳定性 workflow")
	}
	rows, err := database.QueryContext(ctx, `
		SELECT asset_id FROM draft_asset_links WHERE draft_id=? ORDER BY asset_id`, draftID)
	if err != nil {
		return realDraftFacts{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var assetID string
		if err := rows.Scan(&assetID); err != nil {
			return realDraftFacts{}, err
		}
		facts.LinkedAssetIDs[assetID] = true
	}
	if err := rows.Err(); err != nil {
		return realDraftFacts{}, err
	}
	return facts, nil
}

func snapshotSQLiteReadOnly(ctx context.Context, source, target string) error {
	if filepath.Clean(source) == filepath.Clean(target) {
		return errors.New("SQLite 快照目标不能覆盖源数据库")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	database, err := openSQLiteReadOnly(source)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	quotedTarget := "'" + strings.ReplaceAll(target, "'", "''") + "'"
	if _, err := database.ExecContext(ctx, "VACUUM INTO "+quotedTarget); err != nil {
		return err
	}
	return nil
}

func openSQLiteReadOnly(path string) (*sql.DB, error) {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: resolved, RawQuery: "mode=ro"}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := database.Ping(); err != nil {
		_ = database.Close()
		return nil, err
	}
	return database, nil
}

func copyFile(source, target string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = output.Close()
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	closed = true
	return nil
}

func removeRealDraftAttemptWorkspace(root, target string) error {
	resolvedRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	resolvedTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	prefix := resolvedRoot + string(os.PathSeparator)
	if resolvedTarget == resolvedRoot || !strings.HasPrefix(resolvedTarget, prefix) {
		return fmt.Errorf("拒绝删除隔离评测根目录之外的路径: %s", resolvedTarget)
	}
	return os.RemoveAll(resolvedTarget)
}

func decodeRealDraftArguments(raw string) (map[string]any, error) {
	arguments := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &arguments); err != nil {
		return nil, err
	}
	return arguments, nil
}

func requireRealDraftArguments(raw string, expected map[string]any) error {
	actual, err := decodeRealDraftArguments(raw)
	if err != nil {
		return err
	}
	return liveWorkflowMapContains(actual, expected, "arguments")
}

func timelineClipByID(document timeline.Document, clipID string) (timeline.Clip, bool) {
	for _, track := range document.Tracks {
		for _, clip := range track.Clips {
			if clip.TimelineClipID == clipID {
				return clip, true
			}
		}
	}
	return timeline.Clip{}, false
}

func realDraftIndependentAudio(document timeline.Document) map[string][]timeline.Clip {
	result := map[string][]timeline.Clip{}
	for _, track := range document.Tracks {
		switch track.TrackID {
		case "bgm", "sfx", "voiceover":
			result[track.TrackID] = append([]timeline.Clip(nil), track.Clips...)
		}
	}
	return result
}

func realDraftEvalRuns(workflowCount int) int {
	minimum := (realDraftMinimumAttempts + workflowCount - 1) / workflowCount
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("RUSHES_REAL_DRAFT_TOOL_RUNS")))
	if err != nil || value < minimum {
		return minimum
	}
	return min(value, 50)
}

func validateRealDraftToolReport(report realDraftToolReport) error {
	if len(report.Workflows) < realDraftMinimumWorkflows {
		return fmt.Errorf("workflow=%d，至少需要 %d", len(report.Workflows), realDraftMinimumWorkflows)
	}
	if report.Attempted < realDraftMinimumAttempts {
		return fmt.Errorf("attempted=%d，至少需要 %d", report.Attempted, realDraftMinimumAttempts)
	}
	if report.ToolCallsAttempted < realDraftMinimumAttempts ||
		report.ToolCallsSucceeded != report.ToolCallsAttempted {
		return fmt.Errorf(
			"实际工具调用成功=%d/%d，至少需要 %d 且不得存在失败",
			report.ToolCallsSucceeded, report.ToolCallsAttempted, realDraftMinimumAttempts,
		)
	}
	if report.Rate < liveToolStabilityTarget {
		return fmt.Errorf("总体成功率 %.2f%% 低于 %.2f%%", report.Rate*100, liveToolStabilityTarget*100)
	}
	names := make([]string, 0, len(report.Workflows))
	for name := range report.Workflows {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		metric := report.Workflows[name]
		if metric.Total == 0 || metric.Rate < liveToolStabilityTarget {
			return fmt.Errorf(
				"workflow %s 成功率 %.2f%% 低于 %.2f%%",
				name, metric.Rate*100, liveToolStabilityTarget*100,
			)
		}
	}
	return nil
}

func writeRealDraftToolReport(t *testing.T, report realDraftToolReport) {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("RUSHES_REAL_DRAFT_TOOL_REPORT"))
	if path == "" {
		return
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRealDraftGateDefinitionsAndScoring(t *testing.T) {
	t.Parallel()
	workflows := realDraftWorkflows()
	if len(workflows) != realDraftMinimumWorkflows {
		t.Fatalf("workflow=%d want=%d", len(workflows), realDraftMinimumWorkflows)
	}
	seen := map[string]bool{}
	for _, workflow := range workflows {
		if workflow.Name == "" || workflow.ExpectedTool == "" || workflow.Prompt == nil ||
			workflow.ValidateArgs == nil || workflow.Validate == nil {
			t.Fatalf("workflow 定义不完整: %#v", workflow)
		}
		if seen[workflow.Name] {
			t.Fatalf("workflow 重名: %s", workflow.Name)
		}
		seen[workflow.Name] = true
	}
	if got := realDraftEvalRuns(len(workflows)) * len(workflows); got < realDraftMinimumAttempts {
		t.Fatalf("默认尝试数=%d，至少需要 %d", got, realDraftMinimumAttempts)
	}
	passing := realDraftToolReport{
		Attempted: 100, Succeeded: 99, Rate: 0.99,
		ToolCallsAttempted: 100, ToolCallsSucceeded: 100,
		Workflows: map[string]liveToolEvalMetric{},
	}
	for _, workflow := range workflows {
		passing.Workflows[workflow.Name] = liveToolEvalMetric{Succeeded: 20, Total: 20, Rate: 1}
	}
	if err := validateRealDraftToolReport(passing); err != nil {
		t.Fatalf("99%% report=%v", err)
	}
	failing := passing
	failing.Workflows = map[string]liveToolEvalMetric{}
	for name, metric := range passing.Workflows {
		failing.Workflows[name] = metric
	}
	failing.Workflows[workflows[0].Name] = liveToolEvalMetric{Succeeded: 19, Total: 20, Rate: 0.95}
	if err := validateRealDraftToolReport(failing); err == nil {
		t.Fatal("单 workflow 低于 99% 不得通过")
	}
}

func TestRealDraftAttemptRunsMutationWithAgentLeaseAndDurableReceipt(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	const draftID = "draft_real_gate_harness"
	agenttest.CreateAgentDraft(t, database, draftID)
	if err := seedLiveWorkflowVideo(
		t.Context(), service, draftID, "asset_real_gate_harness",
		"真实门禁夹具", "真实 门禁", 60, false,
	); err != nil {
		t.Fatal(err)
	}
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "asset_real_gate_harness", AssetKind: "video",
		SourceStartFrame: 0, SourceEndFrame: 60, Role: "b_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	manualContext := rushestools.WithTimelineMutationOrigin(t.Context(), "manual")
	if result, persistErr := seedTimelineVersion(
		service, manualContext, draftID, document, "real_gate_fixture", nil,
	); persistErr != nil || result.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("seed timeline result=%#v err=%v", result, persistErr)
	}

	goldenDB := filepath.Join(t.TempDir(), "rushes.db")
	if err := snapshotSQLiteReadOnly(t.Context(), database.Paths.DB, goldenDB); err != nil {
		t.Fatal(err)
	}
	workflow := realDraftWorkflows()[2]
	facts := realDraftFacts{
		DraftID: draftID, TimelineID: document.TimelineID,
		DeleteStartFrame: 1, DeleteEndFrame: 2,
		LinkedAssetIDs: map[string]bool{"asset_real_gate_harness": true},
	}
	stub := &scriptedWorkflowModel{calls: []schema.ToolCall{{
		ID: "call_real_gate_delete",
		Function: schema.FunctionCall{
			Name:      "timeline.delete",
			Arguments: `{"kind":"delete_range","start_frame":1,"end_frame":2}`,
		},
	}}}
	attemptWorkspace := filepath.Join(t.TempDir(), "attempt")
	toolCalls, err := runRealDraftToolAttempt(
		t.Context(), attemptWorkspace, goldenDB,
		facts,
		workflow, stub, stub,
	)
	if err != nil || toolCalls != 1 || stub.next != 1 {
		t.Fatalf("attempt calls=%d model=%d err=%v", toolCalls, stub.next, err)
	}

	resultDB, err := openSQLiteReadOnly(filepath.Join(attemptWorkspace, "rushes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resultDB.Close() }()
	var versions, receipts, liveLeases, finishedTurns int
	if err := resultDB.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?", draftID,
	).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if err := resultDB.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM agent_tool_receipts WHERE draft_id=?", draftID,
	).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := resultDB.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM agent_edit_leases WHERE draft_id=?", draftID,
	).Scan(&liveLeases); err != nil {
		t.Fatal(err)
	}
	if err := resultDB.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM agent_turn_runs WHERE draft_id=? AND status='finished'`, draftID,
	).Scan(&finishedTurns); err != nil {
		t.Fatal(err)
	}
	if versions != 2 || receipts != 1 || liveLeases != 0 || finishedTurns != 1 {
		t.Fatalf(
			"versions=%d receipts=%d leases=%d finished_turns=%d",
			versions, receipts, liveLeases, finishedTurns,
		)
	}

	missingIDStub := &scriptedWorkflowModel{calls: []schema.ToolCall{{
		Function: schema.FunctionCall{
			Name:      "timeline.delete",
			Arguments: `{"kind":"delete_range","start_frame":1,"end_frame":2}`,
		},
	}}}
	missingIDWorkspace := filepath.Join(t.TempDir(), "attempt-missing-id")
	toolCalls, err = runRealDraftToolAttempt(
		t.Context(), missingIDWorkspace, goldenDB, facts,
		workflow, missingIDStub, missingIDStub,
	)
	if err == nil || toolCalls != 1 || missingIDStub.next != 1 ||
		!strings.Contains(err.Error(), "缺少 provider tool_call_id") {
		t.Fatalf(
			"missing-id attempt calls=%d model=%d err=%v",
			toolCalls, missingIDStub.next, err,
		)
	}
	missingIDDB, err := openSQLiteReadOnly(filepath.Join(missingIDWorkspace, "rushes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = missingIDDB.Close() }()
	var unchangedVersions, missingReceipts, leakedLeases, failedTurns int
	if err := missingIDDB.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?", draftID,
	).Scan(&unchangedVersions); err != nil {
		t.Fatal(err)
	}
	if err := missingIDDB.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM agent_tool_receipts WHERE draft_id=?", draftID,
	).Scan(&missingReceipts); err != nil {
		t.Fatal(err)
	}
	if err := missingIDDB.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM agent_edit_leases WHERE draft_id=?", draftID,
	).Scan(&leakedLeases); err != nil {
		t.Fatal(err)
	}
	if err := missingIDDB.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM agent_turn_runs WHERE draft_id=? AND status='failed'`, draftID,
	).Scan(&failedTurns); err != nil {
		t.Fatal(err)
	}
	if unchangedVersions != 1 || missingReceipts != 0 || leakedLeases != 0 || failedTurns != 1 {
		t.Fatalf(
			"missing-id versions=%d receipts=%d leases=%d failed_turns=%d",
			unchangedVersions, missingReceipts, leakedLeases, failedTurns,
		)
	}
}
