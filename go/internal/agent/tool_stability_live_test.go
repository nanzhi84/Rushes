package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/providers"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

const liveToolStabilityTarget = 0.95

type liveToolEvalCase struct {
	Name     string
	Prompt   string
	Expected []string
	Snapshot WorldStateSnapshot
}

type liveToolEvalFailure struct {
	Suite    string `json:"suite"`
	Case     string `json:"case"`
	Run      int    `json:"run"`
	Expected string `json:"expected"`
	Actual   string `json:"actual,omitempty"`
	Error    string `json:"error,omitempty"`
}

type liveToolEvalMetric struct {
	Succeeded int     `json:"succeeded"`
	Total     int     `json:"total"`
	Rate      float64 `json:"rate"`
}

type liveToolEvalReport struct {
	GeneratedAt string                `json:"generated_at"`
	Model       string                `json:"model"`
	Schema      liveToolEvalMetric    `json:"schema"`
	Routing     liveToolEvalMetric    `json:"routing"`
	Failures    []liveToolEvalFailure `json:"failures,omitempty"`
}

func TestLiveToolCallingStability(t *testing.T) {
	if os.Getenv("RUSHES_LIVE_TOOL_EVAL") != "1" {
		t.Skip("设置 RUSHES_LIVE_TOOL_EVAL=1 才运行真实模型工具稳定性评测")
	}
	key := strings.TrimSpace(os.Getenv("RUSHES_DASHSCOPE_API_KEY"))
	if key == "" {
		t.Fatal("真实工具评测缺少 RUSHES_DASHSCOPE_API_KEY")
	}
	modelName := strings.TrimSpace(os.Getenv("RUSHES_QWEN_CHAT_MODEL"))
	if modelName == "" {
		modelName = providers.DefaultChatModel
	}
	tiers, err := providers.NewQwenTiers(t.Context(), providers.QwenTierConfig{
		APIKey: key, BaseURL: os.Getenv("RUSHES_DASHSCOPE_BASE_URL"), ChatModel: modelName,
	})
	if err != nil {
		t.Fatal(err)
	}
	database := agentTestDatabase(t)
	service, err := NewServiceWithModels(t.Context(), database, tiers.Chat, tiers.Vision)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	toolInfos := map[string]*schema.ToolInfo{}
	specs := map[string]rushestools.Spec{}
	allInfos := make([]*schema.ToolInfo, 0)
	for _, spec := range service.tools.Specs(true) {
		if spec.Exposure != rushestools.ExposureLLM {
			continue
		}
		info, infoErr := spec.Implementation.Info(t.Context())
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		toolInfos[spec.Name] = info
		specs[spec.Name] = spec
		allInfos = append(allInfos, info)
	}

	report := liveToolEvalReport{GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), Model: modelName}
	runs := liveEvalRuns()
	for _, evalCase := range liveSchemaCases() {
		info := toolInfos[evalCase.Expected[0]]
		if info == nil {
			t.Fatalf("评测工具未注册: %s", evalCase.Expected[0])
		}
		bound, bindErr := tiers.Chat.WithTools([]*schema.ToolInfo{info})
		if bindErr != nil {
			t.Fatal(bindErr)
		}
		for run := 1; run <= runs; run++ {
			report.Schema.Total++
			call, callErr := liveGenerateToolCall(
				t.Context(), bound, evalCase.Prompt, evalCase.Snapshot,
				true, evalCase.Expected[0],
			)
			if callErr == nil {
				callErr = validateLiveToolArguments(specs[evalCase.Expected[0]], call.Function.Arguments)
			}
			if callErr == nil && call.Function.Name == evalCase.Expected[0] {
				report.Schema.Succeeded++
				continue
			}
			report.Failures = append(report.Failures, liveToolEvalFailure{
				Suite: "schema", Case: evalCase.Name, Run: run,
				Expected: evalCase.Expected[0], Actual: call.Function.Name,
				Error: errorText(callErr),
			})
		}
	}

	boundAll, err := tiers.Chat.WithTools(allInfos)
	if err != nil {
		t.Fatal(err)
	}
	for _, evalCase := range liveRoutingCases() {
		for run := 1; run <= runs; run++ {
			report.Routing.Total++
			call, callErr := liveGenerateToolCall(
				t.Context(), boundAll, evalCase.Prompt, evalCase.Snapshot, false, "",
			)
			if callErr == nil && containsToolName(evalCase.Expected, call.Function.Name) {
				callErr = validateLiveToolArguments(specs[call.Function.Name], call.Function.Arguments)
			}
			if callErr == nil && containsToolName(evalCase.Expected, call.Function.Name) {
				report.Routing.Succeeded++
				continue
			}
			report.Failures = append(report.Failures, liveToolEvalFailure{
				Suite: "routing", Case: evalCase.Name, Run: run,
				Expected: strings.Join(evalCase.Expected, "|"), Actual: call.Function.Name,
				Error: errorText(callErr),
			})
		}
	}
	report.Schema.Rate = ratio(report.Schema.Succeeded, report.Schema.Total)
	report.Routing.Rate = ratio(report.Routing.Succeeded, report.Routing.Total)
	writeLiveToolEvalReport(t, report)
	t.Logf(
		"TOOL_STABILITY_RESULT model=%s schema=%d/%d(%.2f%%) routing=%d/%d(%.2f%%) failures=%d",
		report.Model, report.Schema.Succeeded, report.Schema.Total, report.Schema.Rate*100,
		report.Routing.Succeeded, report.Routing.Total, report.Routing.Rate*100, len(report.Failures),
	)
	if report.Schema.Rate < liveToolStabilityTarget || report.Routing.Rate < liveToolStabilityTarget {
		encoded, _ := json.Marshal(report.Failures)
		t.Fatalf("真实工具调用稳定性低于 %.0f%%: %s", liveToolStabilityTarget*100, encoded)
	}
}

func liveSchemaCases() []liveToolEvalCase {
	cases := []liveToolEvalCase{
		{Name: "asset_list", Prompt: "请调用工具列出当前草稿最多 50 个可用素材。", Expected: []string{"asset.list_assets"}},
		{Name: "understand", Prompt: "请深度理解 asset_video_1 和 asset_video_2，重点关注人物动作，每个素材最多 8 段证据。", Expected: []string{"understand.materials"}},
		{Name: "shot_search", Prompt: "请只检索适合覆盖‘指纹解锁位于键盘右上角’这句口播的 B-roll 镜头，最多返回 8 个。", Expected: []string{"media.search_shots"}},
		{Name: "beats", Prompt: "请分析 BGM 素材 asset_bgm_1 的节拍，最多返回 512 个拍点。", Expected: []string{"audio.analyze_beats"}},
		{Name: "speech_pauses", Prompt: "请分析时间线片段 clip_v1_001 的口播气口，阈值 -35dB，最多 100 个候选。", Expected: []string{"audio.analyze_speech_pauses"}},
		{Name: "speech_inspect", Prompt: "请读取 clip_v1_001 的持久化逐句口播索引，检索‘指纹解锁’，同时返回气口和相似台词证据。", Expected: []string{"speech.inspect"}},
		{Name: "ask_user", Prompt: "我们还不知道用户要电影感还是快节奏，请发出一张允许自由输入的阻塞性二选一决策卡。", Expected: []string{"interaction.ask_user"}},
		{Name: "decision_answer", Prompt: "请提交决策 decision_style_1 的答案 option_id=fast，补充说明为强节奏。", Expected: []string{"decision.answer"}},
		{Name: "plan_update", Prompt: "请把已确定的创作计划持久记录下来：风格是克制电影感，节奏决定为前缓后快；使用增量合并，不要整体重置。", Expected: []string{"plan.update"}},
		{Name: "compose", Prompt: "请立即组装初版时间线：asset_video_1 使用源 0到90帧，asset_video_2 使用源 30到120帧，两段都是 b_roll。", Expected: []string{"timeline.compose_initial"}},
		{Name: "single_patch", Prompt: "请将时间线片段 clip_v1_001 的结尾裁到第 75 帧，使用单个语义补丁。", Expected: []string{"timeline.apply_patch"}},
		{Name: "batch_patch", Prompt: "请一次原子调整两段：clip_v1_001 淡出 8 帧，clip_v1_002 淡出 10 帧。", Expected: []string{"timeline.apply_patches"}},
		{Name: "beat_recut", Prompt: "请用 BGM asset_bgm_1 和视频 asset_video_1、asset_video_2 卡点重剪到 1440 帧，覆盖整首音乐，并将 asset_sfx_1 作为 45 帧的独立音效点缀。", Expected: []string{"timeline.recut_to_beats"}},
		{Name: "talking_head_edit", Prompt: "请对 A-roll clip_v1_001 原子执行口播剪辑：删除台词 utt_repeat_1 和气口 pause_2。", Expected: []string{"timeline.edit_talking_head"}},
		{Name: "validate", Prompt: "请校验当前时间线不变量和节拍对齐数据。", Expected: []string{"timeline.validate"}},
		{Name: "inspect", Prompt: "请读取当前时间线的完整轨道、clip ID 和帧范围。", Expected: []string{"timeline.inspect"}},
		{Name: "preview", Prompt: "时间线已验证，请排队生成可分享的预览。", Expected: []string{"render.preview"}},
		{Name: "final", Prompt: "时间线已验证，请排队导出最终 MP4。", Expected: []string{"render.final_mp4"}},
		{Name: "status", Prompt: "请读取当前草稿的渲染任务和产物状态。", Expected: []string{"render.status"}},
		{Name: "inspect_preview", Prompt: "请检查预览 preview_123 的解码、黑帧、静帧、静音和响度。", Expected: []string{"render.inspect_preview"}},
		{Name: "confirm", Prompt: "请为危险的时间线清空操作创建确认：工具 timeline.apply_patch，参数是移除 visual_base 轨道所有片段。", Expected: []string{"interaction.confirm_action"}},
	}
	for index := range cases {
		cases[index].Snapshot = liveSnapshotForSchemaCase(cases[index].Name)
	}
	return cases
}

func liveRoutingCases() []liveToolEvalCase {
	const contextPrefix = `已读取当前客观状态：timeline_fps=30；A-roll asset_aroll_1 已有持久化逐句索引，主视频 clip 为 clip_v1_001；B-roll asset_video_1、asset_video_2 已完成逐镜头理解；BGM asset_bgm_1；SFX asset_sfx_1；当前时间线存在且已验证，预览为 preview_123。`
	cases := []liveToolEvalCase{
		{Name: "route_list", Prompt: contextPrefix + "\n用户：列出当前草稿的所有素材。", Expected: []string{"asset.list_assets"}},
		{Name: "route_understand", Prompt: contextPrefix + "\n用户：素材 ID 已确认，请立即深度理解 asset_video_1 的动作和可剪区间。", Expected: []string{"understand.materials"}},
		{Name: "route_shot_search", Prompt: contextPrefix + "\nspeech.inspect 已返回 utt_fingerprint_1，文本是‘指纹解锁位于键盘右上角’。用户：不用再读取台词，只调用镜头检索找合适的 B-roll，暂时不剪。", Expected: []string{"media.search_shots"}},
		{Name: "route_beats", Prompt: contextPrefix + "\n用户：分析 asset_bgm_1 的节拍和重拍。", Expected: []string{"audio.analyze_beats"}},
		{Name: "route_pauses", Prompt: contextPrefix + "\n用户：不需要逐句 ASR，只对 clip_v1_001 做轻量 RMS 能量静音扫描，暂时不删。", Expected: []string{"audio.analyze_speech_pauses"}},
		{Name: "route_speech_inspect", Prompt: contextPrefix + "\n用户：读取 clip_v1_001 的逐句 ASR，检索重复说到‘指纹解锁’的地方并给出客观相似证据，暂时不删。", Expected: []string{"speech.inspect"}},
		{Name: "route_plan_update", Prompt: contextPrefix + "\n用户：先不要继续剪，把已确定的创作方向记到持久计划本里：整体克制、高潮段加快节奏，供下回合继续。", Expected: []string{"plan.update"}},
		{Name: "route_inspect", Prompt: contextPrefix + "\n用户：查看当前时间线的真实 clip 明细。", Expected: []string{"timeline.inspect"}},
		{Name: "route_patch", Prompt: contextPrefix + "\n用户：已取得真实 ID，只把 clip_v1_001 音量调到 -6dB。", Expected: []string{"timeline.apply_patch"}},
		{Name: "route_batch", Prompt: contextPrefix + "\n用户：已取得真实 ID，一次将 clip_v1_001 和 clip_v1_002 的淡出设为 8 帧。", Expected: []string{"timeline.apply_patches"}},
		{Name: "route_recut", Prompt: contextPrefix + "\n节拍分析已完成，asset_bgm_1 的完整可用长度正好是 1440 帧；音效 asset_sfx_1 已确定从 900 帧开始、持续 45 帧、增益 -12dB，所有创作选择都已确定，无需提问。用户：现在直接覆盖整首 BGM 完成卡点重剪。", Expected: []string{"timeline.recut_to_beats"}},
		{Name: "route_recut_after_recoverable_failure", Prompt: contextPrefix + "\n上一工具结果：{\"status\":\"failed\",\"observation\":\"所选镜头无法覆盖对应节拍片段，或其源区间已被重复使用\",\"data\":{\"shot_id\":\"shot_video_2\",\"required_frames\":120,\"shot_duration_frames\":80,\"recovery\":\"用 media.search_shots 按该片段 min_duration_frames 重新检索，且不要重复传同一 shot_id\"}}。检索已经完成，新候选 shot_video_2b 长 180 帧；原用户目标仍是用既定 BGM、节拍和其余镜头覆盖整首音乐完成卡点成片。请选择下一步工具。", Expected: []string{"timeline.recut_to_beats"}},
		{Name: "route_batch_after_single_patch_failure", Prompt: contextPrefix + "\n上一工具结果：{\"status\":\"failed\",\"observation\":\"时间线补丁字段预校验失败：时间线补丁 trim_clip_edge 的字段 timeline_frame 缺少必填字段\",\"data\":{\"op_kind\":\"trim_clip_edge\",\"invalid_field\":\"timeline_frame\",\"expected_schema\":{\"required\":[\"kind\",\"timeline_clip_id\",\"timeline_frame\",\"edge\"]},\"correct_example\":{\"kind\":\"trim_clip_edge\",\"timeline_clip_id\":\"clip_v1_001\",\"timeline_frame\":75,\"edge\":\"end\"},\"recovery\":\"只修正当前 op 的字段名与类型后重新调用；不要原样重发失败参数。\"}}。字段错误已明确；原用户目标仍是把 clip_v1_001 的结尾裁到 75 帧、clip_v1_002 的结尾裁到 90 帧，两个真实 ID 均已确认。请选择下一步工具。", Expected: []string{"timeline.apply_patches"}},
		{Name: "route_talking_head_edit", Prompt: contextPrefix + "\n逐句和镜头证据已读取，我已选定删除 utt_repeat_1、pause_2，并用 shot_keyboard_1 覆盖 utt_fingerprint_1。请一次原子应用口播剪辑。", Expected: []string{"timeline.edit_talking_head"}},
		{Name: "route_validate", Prompt: contextPrefix + "\n用户：校验时间线和卡点对齐。", Expected: []string{"timeline.validate"}},
		{Name: "route_preview", Prompt: contextPrefix + "\n用户：生成一个可分享的预览。", Expected: []string{"render.preview"}},
		{Name: "route_preview_inspect", Prompt: contextPrefix + "\n用户：质检 preview_123 是否有黑帧、静音和解码问题。", Expected: []string{"render.inspect_preview"}},
		{Name: "route_export", Prompt: contextPrefix + "\n用户：导出最终 MP4，不要只生成预览。", Expected: []string{"render.final_mp4"}},
		{Name: "route_status", Prompt: contextPrefix + "\n用户：查看当前渲染任务的状态。", Expected: []string{"render.status"}},
	}
	snapshot := liveFullTaskSnapshot()
	for index := range cases {
		cases[index].Snapshot = snapshot
	}
	return cases
}

func liveGenerateToolCall(
	parent context.Context,
	chat model.ToolCallingChatModel,
	prompt string,
	snapshot WorldStateSnapshot,
	forced bool,
	allowedName string,
) (schema.ToolCall, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(parent, 90*time.Second)
		options := []model.Option{}
		if forced {
			options = append(options, model.WithToolChoice(schema.ToolChoiceForced, allowedName))
		}
		messages := []*schema.Message{schema.SystemMessage(coreSystemPrompt)}
		if playbook := taskPlaybookMessage(snapshot); playbook != nil {
			messages = append(messages, playbook)
		}
		messages = append(messages, schema.UserMessage(prompt))
		response, err := chat.Generate(ctx, messages, options...)
		cancel()
		if err == nil && response != nil && len(response.ToolCalls) > 0 {
			return response.ToolCalls[0], nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("模型未调用工具，文本回复=%q", truncateText(responseContent(response), 240))
		}
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		}
	}
	return schema.ToolCall{}, lastErr
}

func liveSnapshotForSchemaCase(name string) WorldStateSnapshot {
	assets := map[string]any{
		"audio_roles":      []any{},
		"material_catalog": []any{},
	}
	sections := map[string]any{"assets": assets, "timeline": nil}
	switch name {
	case "beats", "beat_recut":
		assets["audio_roles"] = []any{map[string]any{
			"asset_id": "asset_bgm_1", "suggested_role": "bgm",
		}}
		assets["material_catalog"] = []any{map[string]any{
			"asset_id": "asset_bgm_1", "suggested_role": "bgm",
		}}
	case "speech_inspect", "talking_head_edit":
		assets["material_catalog"] = []any{map[string]any{
			"asset_id": "asset_aroll_1", "transcript_provider": "qwen_asr",
		}}
		sections["timeline"] = map[string]any{"track_count": 1}
	case "single_patch", "batch_patch", "validate", "inspect", "preview", "final",
		"status", "inspect_preview", "confirm":
		sections["timeline"] = map[string]any{"track_count": 1}
	}
	return NewWorldStateSnapshot(sections)
}

func liveFullTaskSnapshot() WorldStateSnapshot {
	return NewWorldStateSnapshot(map[string]any{
		"assets": map[string]any{
			"audio_roles": []any{
				map[string]any{"asset_id": "asset_bgm_1", "suggested_role": "bgm"},
				map[string]any{"asset_id": "asset_sfx_1", "suggested_role": "sfx"},
			},
			"material_catalog": []any{
				map[string]any{"asset_id": "asset_bgm_1", "suggested_role": "bgm"},
				map[string]any{
					"asset_id": "asset_aroll_1", "transcript_provider": "qwen_asr",
				},
			},
		},
		"timeline": map[string]any{"track_count": 3},
	})
}

func validateLiveToolArguments(spec rushestools.Spec, raw string) error {
	if spec.InputType == nil {
		return errors.New("工具没有输入类型")
	}
	target := reflect.New(spec.InputType)
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target.Interface()); err != nil {
		return fmt.Errorf("参数不符合 Go schema: %w", err)
	}
	if input, ok := target.Elem().Interface().(rushestools.TimelinePatchInput); ok {
		if err := validateLiveTimelineOp(input.Op); err != nil {
			return fmt.Errorf("参数不符合 op.oneOf: %w", err)
		}
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		return err
	}
	for index := 0; index < spec.InputType.NumField(); index++ {
		field := spec.InputType.Field(index)
		if !strings.Contains(field.Tag.Get("jsonschema"), "required") {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		value, exists := object[name]
		if !exists || string(value) == "null" || string(value) == `""` || string(value) == "[]" || string(value) == "{}" {
			return fmt.Errorf("缺少必填字段 %s", name)
		}
	}
	return nil
}

func validateLiveTimelineOp(operation rushestools.TimelineOp) error {
	plain := map[string]any(operation)
	if err := timeline.ValidateOpFields(plain); err != nil {
		return err
	}
	kind, _ := plain["kind"].(string)
	spec, exists := timeline.LookupOpSpec(kind)
	if !exists {
		return fmt.Errorf("未知 kind %q", kind)
	}
	allowed := map[string]bool{"kind": true}
	for _, field := range spec.Fields {
		if field.Injected {
			continue
		}
		allowed[field.Name] = true
		for _, alias := range field.Aliases {
			allowed[alias] = true
		}
	}
	for name := range plain {
		if !allowed[name] {
			return fmt.Errorf("kind %s 不公开字段 %s", kind, name)
		}
	}
	return nil
}

func TestValidateLiveToolArgumentsChecksTimelineOpOneOfContract(t *testing.T) {
	t.Parallel()
	spec := rushestools.Spec{InputType: reflect.TypeFor[rushestools.TimelinePatchInput]()}
	valid := []string{
		`{"op":{"kind":"trim_clip_edge","timeline_clip_id":"clip_1","timeline_frame":75,"edge":"end"}}`,
		`{"op":{"kind":"delete_clip","clip_id":"clip_1"}}`,
		`{"op":{"kind":"sync_original_audio"}}`,
		`{"op":{"kind":"set_track_state","track_id":"bgm","muted":true}}`,
	}
	for _, raw := range valid {
		if err := validateLiveToolArguments(spec, raw); err != nil {
			t.Errorf("合法参数被拒绝 raw=%s err=%v", raw, err)
		}
	}
	invalid := []string{
		`{"op":{"kind":"trim_clip_edge","timeline_clip_id":"clip_1","target_frame":75,"edge":"end"}}`,
		`{"op":{"kind":"delete_clip","clip_id":"clip_1","target_frame":75}}`,
		`{"op":{"kind":"insert_clip","asset_id":"asset_1","source_start_frame":0,"source_end_frame":90,"asset_kind":"video"}}`,
		`{"op":{"kind":"set_track_state","track_id":"bgm"}}`,
	}
	for _, raw := range invalid {
		if err := validateLiveToolArguments(spec, raw); err == nil {
			t.Errorf("非法参数错误通过 raw=%s", raw)
		}
	}
}

func liveEvalRuns() int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("RUSHES_TOOL_EVAL_RUNS")))
	if err != nil || value < 1 {
		return 1
	}
	return min(value, 5)
}

func writeLiveToolEvalReport(t *testing.T, report liveToolEvalReport) {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("RUSHES_TOOL_EVAL_REPORT"))
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

func responseContent(response *schema.Message) string {
	if response == nil {
		return ""
	}
	return response.Content
}

func containsToolName(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func ratio(success, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(success) / float64(total)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
