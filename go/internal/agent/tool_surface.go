package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

const (
	maxBoundModelTools       = 10
	maxBoundModelSchemaRunes = 16000
)

type modelToolSurfaceSession struct {
	mu         sync.RWMutex
	configured bool
	allowed    map[string]struct{}
	names      []string
}

type modelToolSurfaceContextKey struct{}

func withModelToolSurfaceSession(ctx context.Context) context.Context {
	return context.WithValue(ctx, modelToolSurfaceContextKey{}, &modelToolSurfaceSession{})
}

func modelToolSurfaceSessionFromContext(ctx context.Context) *modelToolSurfaceSession {
	session, _ := ctx.Value(modelToolSurfaceContextKey{}).(*modelToolSurfaceSession)
	return session
}

func (session *modelToolSurfaceSession) set(names []string) {
	if session == nil {
		return
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	allowed := make(map[string]struct{}, len(sorted))
	for _, name := range sorted {
		allowed[name] = struct{}{}
	}
	session.mu.Lock()
	session.configured = true
	session.allowed = allowed
	session.names = sorted
	session.mu.Unlock()
}

func (session *modelToolSurfaceSession) allows(name string) (bool, bool) {
	if session == nil {
		return true, false
	}
	session.mu.RLock()
	defer session.mu.RUnlock()
	if !session.configured {
		return true, false
	}
	_, ok := session.allowed[name]
	return ok, true
}

func (session *modelToolSurfaceSession) boundNames() []string {
	if session == nil {
		return nil
	}
	session.mu.RLock()
	defer session.mu.RUnlock()
	return append([]string(nil), session.names...)
}

// dynamicToolSurfaceModel 让共享 ReAct 图在每次模型调用前重新从 Registry 派生工具面。
// WithTools 由建图阶段调用；静态目录只交给 ToolsNode 做执行分发，不下发给 provider。
type dynamicToolSurfaceModel struct {
	inner    model.ToolCallingChatModel
	registry *rushestools.Registry
}

func (surface *dynamicToolSurfaceModel) WithTools(
	_ []*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	return surface, nil
}

func (surface *dynamicToolSurfaceModel) Generate(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.Message, error) {
	bound, prepared, err := surface.bind(ctx, messages)
	if err != nil {
		return nil, err
	}
	return bound.Generate(ctx, prepared, options...)
}

func (surface *dynamicToolSurfaceModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	bound, prepared, err := surface.bind(ctx, messages)
	if err != nil {
		return nil, err
	}
	return bound.Stream(ctx, prepared, options...)
}

func (surface *dynamicToolSurfaceModel) bind(
	ctx context.Context,
	messages []*schema.Message,
) (model.ToolCallingChatModel, []*schema.Message, error) {
	specs, err := selectModelToolSurface(ctx, surface.registry, messages)
	if err != nil {
		return nil, nil, err
	}
	if specsRequireTimelineEditLease(specs) {
		session := timelineEditLeaseSessionFromContext(ctx)
		if session == nil {
			return nil, nil, errors.New("动态编辑工具面缺少 edit lease session")
		}
		if err := session.ensure(ctx); err != nil {
			return nil, nil, err
		}
	}
	implementations := implementationsForSpecs(specs)
	infos := make([]*schema.ToolInfo, 0, len(implementations))
	for _, implementation := range implementations {
		info, infoErr := implementation.Info(ctx)
		if infoErr != nil {
			return nil, nil, fmt.Errorf("读取动态工具信息: %w", infoErr)
		}
		infos = append(infos, info)
	}
	bound := surface.inner
	if len(infos) > 0 {
		bound, err = surface.inner.WithTools(infos)
		if err != nil {
			return nil, nil, err
		}
	}
	recordBoundModelToolSurface(ctx, implementations)
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	if session := modelToolSurfaceSessionFromContext(ctx); session != nil {
		session.set(names)
	}
	prepared, err := refreshCurrentTimelineView(ctx, messages)
	if err != nil {
		return nil, nil, err
	}
	return bound, prepared, nil
}

func implementationsForSpecs(specs []rushestools.Spec) []tool.BaseTool {
	implementations := make([]tool.BaseTool, 0, len(specs))
	for _, spec := range specs {
		implementations = append(implementations, spec.Implementation)
	}
	return implementations
}

func selectModelToolSurface(
	ctx context.Context,
	registry *rushestools.Registry,
	messages []*schema.Message,
) ([]rushestools.Spec, error) {
	allowed, err := registry.Allowed(ctx, true)
	if err != nil {
		return nil, err
	}
	userText := latestUserSurfaceText(messages)
	positiveUserText := withoutNegatedSurfaceActions(userText)
	lane := inferModelToolSurface(allowed, messages)
	lane = surfaceWithAvailablePrerequisites(allowed, lane, positiveUserText)
	selected := filterSurface(allowed, lane)
	allowEmpty := false
	if lane == rushestools.SurfaceDiscovery {
		// Discovery 只负责收集素材/镜头证据和必要的控制信息。即使 Registry
		// 元数据以后误把 timeline mutation 标进来，也不能因此在 provider
		// 看到工具前提前取得独占 edit lease。
		selected = filterSpecsWithoutTimelineEditLease(selected)
	}
	// Beat / transcript analysis can share a workflow surface with later edits,
	// but a read-only user request must not expose mutation capabilities merely
	// because the same tools are useful in a future editing round. The lease is
	// acquired only after the user actually asks to change or preview a timeline.
	if requestsReadOnlyMediaAnalysis(positiveUserText) {
		selected = filterSpecsWithoutTimelineEditLease(selected)
		if requestsOnlyBeatAnalysis(positiveUserText) {
			selected = nil
			allowEmpty = true
		}
	}
	// CurrentTimelineView 与 Harness 自动检查已经覆盖只读校验请求。模型只需基于
	// 注入事实回答，不获得 timeline.check，也不因纯读取提前取得 edit lease。
	if requestsReadOnlyTimelineCheck(userText) &&
		!successfulTimelineMutationSinceLatestUser(messages) &&
		!successfulToolCallSinceLatestUser(messages, "preview.generate") {
		selected = nil
		allowEmpty = true
	}
	// “导出/下载最终视频”是用户 UI 能力，不是预览请求。纯导出意图只给模型
	// 一个只读时间线事实入口，用于说明当前版本与引导按钮；不能借 SurfaceRender
	// 暴露 preview.generate，更不能因此提前取得 edit lease。组合剪辑请求仍先走对应
	// 编辑面，完成后由模型按系统边界引导用户点击导出。
	if requestsUserFinalExportOnly(positiveUserText) {
		selected = nil
		allowEmpty = true
	}
	if lane == rushestools.SurfaceControl {
		selected = selectControlToolSurface(allowed, messages)
	}
	if lane == rushestools.SurfaceTimelineEdit && requestsTalkingHeadWorkflow(positiveUserText) {
		// 口播删剪后仍需重新观察 source→timeline 映射和按保留台词检索 B-roll。
		// 这些证据工具与原子编辑共享当前轮工具面，但不会把旧复合编辑带回来。
		selected = append(selected, filterSpecsByName(
			allowed,
			"media.detect_shots",
			"shot.search",
			"speech.search",
		)...)
	}
	if isEditingSurface(lane) && requestsRenderWorkflow(positiveUserText) &&
		successfulTimelineMutationSinceLatestUser(messages) &&
		successfulToolCallSinceLatestUser(messages, "timeline.check") {
		selected = append(selected, filterSpecsByName(allowed, "preview.generate")...)
	}
	if requestsTimelineInspect(positiveUserText) {
		selected = nil
		allowEmpty = true
	}
	if len(selected) == 0 && !allowEmpty && lane != rushestools.SurfaceDiscovery {
		lane = rushestools.SurfaceDiscovery
		selected = filterSurface(allowed, lane)
	}
	if len(selected) == 0 {
		if allowEmpty {
			return nil, nil
		}
		return nil, noModelToolsError(lane)
	}
	metrics, err := modelToolSchemaSizeFromTools(ctx, implementationsForSpecs(selected))
	if err != nil {
		return nil, err
	}
	if len(selected) > maxBoundModelTools || metrics.TotalRunes > maxBoundModelSchemaRunes {
		return nil, fmt.Errorf(
			"动态模型工具面超出预算: surface=%d tools=%d/%d schema_runes=%d/%d",
			lane, len(selected), maxBoundModelTools, metrics.TotalRunes, maxBoundModelSchemaRunes,
		)
	}
	return selected, nil
}

func filterSpecsByName(specs []rushestools.Spec, names ...string) []rushestools.Spec {
	selected := make([]rushestools.Spec, 0, len(names))
	for _, spec := range specs {
		for _, name := range names {
			if spec.Name == name {
				selected = append(selected, spec)
				break
			}
		}
	}
	return selected
}

func filterSpecsWithoutTimelineEditLease(specs []rushestools.Spec) []rushestools.Spec {
	selected := make([]rushestools.Spec, 0, len(specs))
	for _, spec := range specs {
		if !toolRequiresTimelineEditLease(spec.Name) {
			selected = append(selected, spec)
		}
	}
	return selected
}

func noModelToolsError(lane rushestools.Surface) error {
	return fmt.Errorf("当前状态没有可绑定的模型工具: surface=%d", lane)
}

func filterSurface(specs []rushestools.Spec, lane rushestools.Surface) []rushestools.Spec {
	selected := make([]rushestools.Spec, 0, len(specs))
	for _, spec := range specs {
		if spec.Surfaces.Includes(lane) {
			selected = append(selected, spec)
		}
	}
	return selected
}

func selectControlToolSurface(
	specs []rushestools.Spec,
	messages []*schema.Message,
) []rushestools.Spec {
	if pendingDecisionInMessages(messages) {
		return filterSpecsByName(specs, "decision.answer")
	}
	if needsPlanUpdateSurface(messages) {
		return filterSpecsByName(specs, "plan.update")
	}
	if confirmationRequiredSinceLatestUser(messages) {
		return filterSpecsByName(specs, "interaction.confirm_action")
	}
	text := latestUserSurfaceText(messages)
	if containsSurfaceKeyword(text, "确认卡", "破坏性", "confirm_action", "确认操作") {
		return filterSpecsByName(specs, "interaction.confirm_action")
	}
	names := make([]string, 0, 2)
	if containsSurfaceKeyword(text,
		"忘记", "删除长期记忆", "移除记忆", "清除记忆", "memory.remove") {
		names = append(names, "memory.remove")
	} else if containsSurfaceKeyword(text,
		"记住", "长期偏好", "用户画像", "memory.set") {
		names = append(names, "memory.set")
	}
	if containsSurfaceKeyword(text, "更新计划", "plan.update", "plan.") {
		names = append(names, "plan.update")
	}
	if containsSurfaceKeyword(text, "decision.answer", "decision.") {
		names = append(names, "decision.answer")
	}
	return filterSpecsByName(specs, names...)
}

func confirmationRequiredSinceLatestUser(messages []*schema.Message) bool {
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message == nil {
			continue
		}
		if message.Role == schema.User {
			return false
		}
		if message.Role == schema.Tool &&
			toolMessageErrorCode(message.Content) == string(rushestools.ErrCodeConfirmationRequired) {
			return true
		}
	}
	return false
}

func toolMessageErrorCode(content string) string {
	var payload map[string]any
	if json.Unmarshal([]byte(content), &payload) != nil {
		return ""
	}
	key := "error_" + "code"
	if value, _ := payload[key].(string); value != "" {
		return value
	}
	data, _ := payload["data"].(map[string]any)
	value, _ := data[key].(string)
	return value
}

func pendingDecisionInMessages(messages []*schema.Message) bool {
	pending := false
	initialized := false
	for _, message := range messages {
		if message == nil || message.Role != schema.System {
			continue
		}
		phase, _ := message.Extra["context_phase"].(string)
		if phase != "world_state_reference" && phase != "world_state_update" {
			continue
		}
		payload, ok := worldStateMessagePayload(message.Content)
		if !ok {
			continue
		}
		sections, _ := payload["sections"].(map[string]any)
		if phase == "world_state_reference" {
			pending = false
			initialized = true
		}
		draftValue, draftPresent := sections["draft"]
		if !draftPresent {
			continue
		}
		if draftValue == nil {
			pending = false
			initialized = true
			continue
		}
		draft, _ := draftValue.(map[string]any)
		value, present := draft["pending_decision_id"]
		if !present {
			continue
		}
		decisionID, _ := value.(string)
		pending = strings.TrimSpace(decisionID) != ""
		initialized = true
	}
	return initialized && pending
}

func worldStateMessagePayload(content string) (map[string]any, bool) {
	_, raw, found := strings.Cut(content, "\n")
	if !found {
		return nil, false
	}
	if line, _, hasMore := strings.Cut(raw, "\n"); hasMore {
		raw = line
	}
	var payload map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &payload) != nil {
		return nil, false
	}
	return payload, true
}

func latestUserSurfaceText(messages []*schema.Message) string {
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message == nil || message.Role != schema.User {
			continue
		}
		content := strings.TrimSpace(message.Content)
		if content != "" && !isDecisionContinuationSurfaceMessage(content) {
			return strings.ToLower(content)
		}
	}
	return ""
}

func isDecisionContinuationSurfaceMessage(content string) bool {
	return strings.Contains(content, "这是同一条任务的继续，不是新的请求。")
}

func inferModelToolSurface(
	specs []rushestools.Spec,
	messages []*schema.Message,
) rushestools.Surface {
	text := withoutNegatedSurfaceActions(latestUserSurfaceText(messages))
	if pendingDecisionInMessages(messages) {
		return rushestools.SurfaceControl
	}
	if needsPlanUpdateSurface(messages) {
		return rushestools.SurfaceControl
	}
	if recent := recentSuccessfulWorkflowSurface(messages, text); recent != 0 {
		return recent
	}
	lastIndex := -1
	lastEditIndex := -1
	var explicit rushestools.Surface
	var explicitEdit rushestools.Surface
	for _, spec := range specs {
		index := strings.LastIndex(text, strings.ToLower(spec.Name))
		if index > lastIndex {
			lastIndex = index
			explicit = spec.PrimarySurface
		}
		if index > lastEditIndex &&
			spec.Family == rushestools.FamilyEdit &&
			isEditingSurface(spec.PrimarySurface) {
			lastEditIndex = index
			explicitEdit = spec.PrimarySurface
		}
	}
	if (explicit == rushestools.SurfaceRender ||
		explicit == rushestools.SurfacePreviewCheck) &&
		explicitEdit != 0 {
		return explicitEdit
	}
	// “渲染新预览并质检”必须先精确渲染当前 timeline_id。草稿中可能仍有旧
	// preview；只有本轮 preview.generate 返回成功产物后，后续轮次才推进 PreviewCheck。
	if explicit == rushestools.SurfacePreviewCheck && requestsRenderWorkflow(text) {
		return rushestools.SurfaceRender
	}
	if explicit != 0 {
		return explicit
	}
	switch {
	case containsSurfaceKeyword(text,
		"记住", "忘记", "长期偏好", "用户画像", "memory.", "更新计划", "plan.",
		"确认卡", "破坏性", "decision.", "confirm_action"):
		return rushestools.SurfaceControl
	case pendingEditingSurface(text) != 0:
		return pendingEditingSurface(text)
	case requestsRenderWorkflow(text):
		return rushestools.SurfaceRender
	case requestsPreviewCheck(text):
		return rushestools.SurfacePreviewCheck
	case requestsTalkingHeadWorkflow(text):
		return rushestools.SurfaceTalkingHead
	case containsSurfaceKeyword(text, "卡点", "拍点", "节拍", "音频", "bpm", "bgm", "beat"):
		return rushestools.SurfaceBeatEdit
	case containsSurfaceKeyword(text,
		"组装初版时间线", "建立时间线", "创建时间线", "初版时间线", "首剪"):
		return rushestools.SurfaceDiscovery
	case requestsAssetSearchForTimelineEdit(text):
		return rushestools.SurfaceDiscovery
	case strings.Contains(text, "时间线"):
		return rushestools.SurfaceTimelineEdit
	case containsSurfaceKeyword(text,
		"有哪些素材", "查看素材", "列出素材", "素材列表", "理解素材",
		"asset.", "shot") || requestsShotSearch(text):
		return rushestools.SurfaceDiscovery
	default:
		// 空时间线时只有 timeline.insert 可用；宽泛请求仍先在 Discovery
		// 获取素材/镜头事实。只有已有时间线（delete/update/split 至少一个可用）
		// 或用户明确点名原子编辑时，才进入 TimelineEdit。
		if hasAnyAllowedTool(specs, "timeline.delete", "timeline.update", "timeline.split") {
			return rushestools.SurfaceTimelineEdit
		}
		return rushestools.SurfaceDiscovery
	}
}

func isEditingSurface(surface rushestools.Surface) bool {
	return surface == rushestools.SurfaceTalkingHead ||
		surface == rushestools.SurfaceBeatEdit ||
		surface == rushestools.SurfaceTimelineEdit
}

func needsPlanUpdateSurface(messages []*schema.Message) bool {
	if successfulToolCallSinceLatestUser(messages, "plan.update") {
		return false
	}
	for _, message := range messages {
		if message == nil || message.Role != schema.System {
			continue
		}
		if strings.Contains(message.Content, "【工具预算提醒】") &&
			strings.Contains(message.Content, "先用 plan.update 固化") {
			return true
		}
		if strings.Contains(message.Content, "【上下文压缩提醒】") &&
			strings.Contains(message.Content, "先用 plan.update") {
			return true
		}
	}
	return false
}

func successfulToolCallSinceLatestUser(messages []*schema.Message, toolName string) bool {
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message == nil {
			continue
		}
		if message.Role == schema.User {
			if isDecisionContinuationSurfaceMessage(message.Content) {
				continue
			}
			return false
		}
		if message.Role == schema.Tool && message.ToolName == toolName {
			return workflowToolCallSucceeded(message)
		}
		if toolName == "timeline.check" && message.Role == schema.Tool &&
			isTerminalTimelineMutation(message.ToolName) && automaticTimelineCheckSucceeded(message) {
			return true
		}
	}
	return false
}

func surfaceWithAvailablePrerequisites(
	specs []rushestools.Spec,
	lane rushestools.Surface,
	text string,
) rushestools.Surface {
	switch lane {
	case rushestools.SurfaceTalkingHead:
		if !hasAnyAllowedTool(specs,
			"speech.search",
			"shot.search",
			"timeline.insert",
		) {
			return rushestools.SurfaceDiscovery
		}
	case rushestools.SurfaceBeatEdit:
		if !hasAnyAllowedTool(specs,
			"shot.search",
			"timeline.insert",
			"timeline.delete",
			"timeline.update",
			"timeline.split",
		) {
			return rushestools.SurfaceDiscovery
		}
	case rushestools.SurfaceTimelineEdit:
		hasExistingTimelineEdits := hasAnyAllowedTool(
			specs,
			"timeline.delete",
			"timeline.update",
			"timeline.split",
		)
		if !hasExistingTimelineEdits &&
			hasAllowedTool(specs, "timeline.insert") &&
			!requestsTimelineInspect(text) &&
			!strings.Contains(text, "timeline.insert") &&
			!requestsTimelineMutation(text) &&
			!requestsInitialTimelineComposition(text) &&
			!requestsAssetSearchForTimelineEdit(text) {
			return rushestools.SurfaceDiscovery
		}
		if !hasAnyAllowedTool(specs,
			"timeline.insert",
			"timeline.delete",
			"timeline.update",
			"timeline.split",
		) && !requestsTimelineInspect(text) {
			return rushestools.SurfaceDiscovery
		}
	case rushestools.SurfaceRender:
		if !hasAllowedTool(specs, "preview.generate") {
			return rushestools.SurfaceDiscovery
		}
	case rushestools.SurfacePreviewCheck:
		if !hasAllowedTool(specs, "preview.check") {
			if hasAllowedTool(specs, "preview.generate") {
				return rushestools.SurfaceRender
			}
			return rushestools.SurfaceDiscovery
		}
	}
	return lane
}

func requestsTimelineInspect(text string) bool {
	if !strings.Contains(text, "timeline.inspect") &&
		!containsSurfaceKeyword(text, "读取时间线", "读取当前时间线", "查看时间线", "查看当前时间线") {
		return false
	}
	return !containsSurfaceKeyword(text,
		"剪辑", "剪掉", "裁剪", "裁到", "分割", "移动片段", "淡入", "淡出",
		"音量", "字幕", "编辑", "修改", "调整", "patch", "渲染", "导出", "质检",
	)
}

func requestsReadOnlyTimelineCheck(text string) bool {
	if !requestsTimelineCheckIntent(text) {
		return false
	}
	positiveActions := withoutNegatedSurfaceActions(text)
	if requestsTimelineMutation(positiveActions) ||
		requestsInitialTimelineComposition(positiveActions) ||
		requestsAssetSearchForTimelineEdit(positiveActions) ||
		requestsRenderWorkflow(positiveActions) ||
		requestsPreviewCheck(positiveActions) ||
		requestsUserFinalExport(positiveActions) {
		return false
	}
	// “分析 BGM/转写口播后再校验”仍是复合工作流；单纯检查节拍对齐或口播
	// 时间线约束则由 timeline.check 自身完成，不需要暴露分析或编辑工具。
	if containsSurfaceKeyword(positiveActions, "分析", "检测", "识别", "转写", "搜索", "检索") &&
		(requestsBeatEditWorkflow(positiveActions) || requestsTalkingHeadWorkflow(positiveActions)) {
		return false
	}
	return true
}

func requestsTimelineCheckIntent(text string) bool {
	return strings.Contains(text, "timeline.check") ||
		containsSurfaceKeyword(text,
			"校验时间线", "校验当前时间线",
			"检查时间线", "检查当前时间线",
			"验证时间线", "验证当前时间线",
			"时间线校验", "时间线检查", "时间线验证",
			"检查稳定版本", "校验稳定版本", "验证稳定版本",
		)
}

var negatableSurfaceActions = []string{
	"组装初版时间线", "建立时间线", "创建时间线", "初版时间线",
	"生成预览", "可分享预览", "preview.generate", "最终成片", "最终视频",
	"移动片段", "搜索镜头", "查找镜头", "检索镜头", "镜头检索",
	"同步到", "渲染成片", "离线画质", "预览质检",
	"剪辑", "剪掉", "裁剪", "裁到", "分割", "淡入", "淡出", "编辑", "修改", "调整",
	"加到", "放到", "放进", "插入", "添加", "替换", "铺到", "首剪",
	"渲染", "预览", "导出", "下载", "mp4", "质检", "黑帧", "静帧", "静音", "响度", "解码",
	"分析", "检测", "识别", "转写", "搜索", "检索", "clip", "patch",
}

func withoutNegatedSurfaceActions(text string) string {
	// 只删除处于明确否定作用域内的动作词，不删除整句。这样“无需编辑，只需渲染
	// 预览并校验”会保留正向渲染，而“不要编辑或渲染”中的两个动作都会被移除。
	var positive strings.Builder
	for index := 0; index < len(text); {
		matched := ""
		for _, action := range negatableSurfaceActions {
			if strings.HasPrefix(text[index:], action) && len(action) > len(matched) {
				matched = action
			}
		}
		if matched != "" && surfaceActionIsNegated(text, index) {
			index += len(matched)
			continue
		}
		positive.WriteByte(text[index])
		index++
	}
	return positive.String()
}

func surfaceActionIsNegated(text string, actionStart int) bool {
	clauseStart := 0
	for index, character := range text[:actionStart] {
		switch character {
		case '，', ',', '；', ';', '。', '.', '！', '!', '\n', '\r':
			clauseStart = index + len(string(character))
		}
	}
	prefix := text[clauseStart:actionStart]
	lastNegative := lastSurfaceKeywordIndex(prefix,
		"不要", "不需要", "无需", "不必", "不得", "不能", "禁止", "暂不", "别再", "请别",
	)
	trimmedPrefix := strings.TrimSpace(prefix)
	if strings.HasPrefix(trimmedPrefix, "别") && lastNegative < 0 {
		lastNegative = 0
	}
	if strings.HasSuffix(trimmedPrefix, "别") {
		lastNegative = len(prefix)
	}
	if strings.HasSuffix(trimmedPrefix, "不") {
		lastNegative = len(prefix)
	}
	if lastNegative < 0 {
		return false
	}
	lastPositivePivot := lastSurfaceKeywordIndex(prefix,
		"而是", "只需", "只要", "然后", "随后", "接着", "改为", "转而", "即可",
		"直接", "立即", "马上", "但是", "不过", "但",
	)
	if lastPositivePivot > lastNegative && containsSurfaceKeyword(
		prefix[lastNegative:lastPositivePivot], negatableSurfaceActions...,
	) {
		return false
	}
	return true
}

func lastSurfaceKeywordIndex(text string, keywords ...string) int {
	latest := -1
	for _, keyword := range keywords {
		if index := strings.LastIndex(text, keyword); index > latest {
			latest = index
		}
	}
	return latest
}

func hasAllowedTool(specs []rushestools.Spec, name string) bool {
	for _, spec := range specs {
		if spec.Name == name {
			return true
		}
	}
	return false
}

func hasAnyAllowedTool(specs []rushestools.Spec, names ...string) bool {
	for _, name := range names {
		if hasAllowedTool(specs, name) {
			return true
		}
	}
	return false
}

func recentSuccessfulWorkflowSurface(
	messages []*schema.Message,
	userText string,
) rushestools.Surface {
	if requestsInitialTimelineComposition(userText) &&
		!successfulTimelineMutationSinceLatestUser(messages) {
		// 首剪先完成两类只读证据，再进入会取得 edit lease 的原子写阶段。
		// 两个读取允许并行或任意顺序，不能因其中一个先完成就把另一个移出工具面。
		if successfulToolCallSinceLatestUser(messages, "asset.list_assets") &&
			successfulToolCallSinceLatestUser(messages, "shot.search") {
			return rushestools.SurfaceTimelineEdit
		}
		return rushestools.SurfaceDiscovery
	}
	seen := make(map[string]struct{})
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message == nil {
			continue
		}
		if message.Role == schema.User {
			if isDecisionContinuationSurfaceMessage(message.Content) {
				continue
			}
			return 0
		}
		if message.Role != schema.Tool {
			continue
		}
		if !isWorkflowTransitionTool(message.ToolName) {
			continue
		}
		if _, exists := seen[message.ToolName]; exists {
			continue
		}
		seen[message.ToolName] = struct{}{}
		if !workflowToolCallSucceeded(message) {
			continue
		}
		switch message.ToolName {
		case "plan.update", "memory.set", "memory.remove":
			return remainingWorkflowSurface(userText)
		case "asset.list_assets":
			return remainingWorkflowSurface(userText)
		case "media.detect_shots":
			return remainingWorkflowSurface(userText)
		case "speech.search":
			if requestsTalkingHeadWorkflow(userText) {
				return rushestools.SurfaceTimelineEdit
			}
		case "shot.search":
			if requestsTalkingHeadWorkflow(userText) {
				if successfulToolCallSinceLatestUser(messages, "speech.search") {
					return rushestools.SurfaceTimelineEdit
				}
				return rushestools.SurfaceTalkingHead
			}
			if requestsBeatEditWorkflow(userText) {
				return rushestools.SurfaceBeatEdit
			}
			if requestsAssetSearchForTimelineEdit(userText) {
				return rushestools.SurfaceTimelineEdit
			}
		case "timeline.insert", "timeline.delete", "timeline.update", "timeline.split":
			if requestsBeatEditWorkflow(userText) {
				return rushestools.SurfaceBeatEdit
			}
			return rushestools.SurfaceTimelineEdit
		case "preview.generate":
			if requestsPreviewCheck(userText) {
				return rushestools.SurfacePreviewCheck
			}
			return rushestools.SurfaceRender
		case "preview.check":
			// preview.check 是只读证据收集，但其终态结果可能要求模型继续修正
			// 时间线。检查完成后回到原子编辑面，避免更早的 preview.generate
			// 一直把后续 provider 调用锁在只能继续质检的工具面。
			return rushestools.SurfaceTimelineEdit
		}
	}
	return 0
}

func automaticTimelineCheckSucceeded(message *schema.Message) bool {
	if message == nil || message.Role != schema.Tool || !isTerminalTimelineMutation(message.ToolName) {
		return false
	}
	var result struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if json.Unmarshal([]byte(message.Content), &result) != nil {
		return false
	}
	var check rushestools.ToolResult
	return json.Unmarshal(result.Data[automaticTimelineCheckDataKey], &check) == nil &&
		check.Status == string(rushestools.StatusSucceeded) &&
		isValidTimelineVersionID(agentexec.InterfaceString(check.Data["timeline_id"]))
}

func successfulTimelineMutationSinceLatestUser(messages []*schema.Message) bool {
	for _, name := range []string{
		"timeline.insert", "timeline.delete", "timeline.update", "timeline.split",
	} {
		if successfulToolCallSinceLatestUser(messages, name) {
			return true
		}
	}
	return false
}

func isWorkflowTransitionTool(name string) bool {
	switch name {
	case "plan.update",
		"memory.set",
		"memory.remove",
		"asset.list_assets",
		"media.detect_shots",
		"speech.search",
		"shot.search",
		"timeline.insert",
		"timeline.delete",
		"timeline.update",
		"timeline.split",
		"preview.generate",
		"preview.check":
		return true
	default:
		return false
	}
}

func remainingWorkflowSurface(text string) rushestools.Surface {
	switch {
	case requestsInitialTimelineComposition(text):
		return rushestools.SurfaceTimelineEdit
	case pendingEditingSurface(text) != 0:
		return pendingEditingSurface(text)
	case requestsRenderWorkflow(text):
		return rushestools.SurfaceRender
	case requestsPreviewCheck(text):
		return rushestools.SurfacePreviewCheck
	case requestsTalkingHeadWorkflow(text):
		return rushestools.SurfaceTalkingHead
	case containsSurfaceKeyword(text, "卡点", "拍点", "节拍", "音频", "bpm", "bgm", "beat"):
		return rushestools.SurfaceBeatEdit
	case requestsAssetSearchForTimelineEdit(text):
		return rushestools.SurfaceDiscovery
	default:
		return 0
	}
}

func requestsInitialTimelineComposition(text string) bool {
	return containsSurfaceKeyword(text,
		"组装初版时间线", "建立时间线", "创建时间线", "初版时间线", "首剪",
		"做一个完整短片", "做个完整短片", "做一个短片", "做个短片",
	)
}

func pendingEditingSurface(text string) rushestools.Surface {
	switch {
	case requestsTalkingHeadWorkflow(text) &&
		(requestsTimelineMutation(text) || containsSurfaceKeyword(text,
			"清理", "剪辑", "剪掉", "删除", "去掉", "修剪", "编辑", "修改", "处理")):
		return rushestools.SurfaceTalkingHead
	case requestsBeatEditWorkflow(text) && requestsTimelineMutation(text):
		return rushestools.SurfaceBeatEdit
	case containsSurfaceKeyword(text, "卡点", "踩点", "对齐拍点", "按节拍剪", "节拍重剪"):
		return rushestools.SurfaceBeatEdit
	case containsSurfaceKeyword(text,
		"组装初版时间线", "建立时间线", "创建时间线", "初版时间线", "首剪"):
		return rushestools.SurfaceDiscovery
	case requestsAssetSearchForTimelineEdit(text):
		return rushestools.SurfaceDiscovery
	case requestsTimelineMutation(text):
		return rushestools.SurfaceTimelineEdit
	default:
		return 0
	}
}

func requestsTimelineMutation(text string) bool {
	if containsSurfaceKeyword(text,
		"剪辑", "剪掉", "裁剪", "裁到", "分割", "移动片段", "淡入", "淡出",
		"编辑", "修改", "调整", "clip", "patch",
	) {
		return true
	}
	if strings.Contains(text, "音量") &&
		containsSurfaceKeyword(text, "调高", "调低", "增大", "降低", "修改", "调整") {
		return true
	}
	if containsSurfaceKeyword(text, "时间线", "轨道") &&
		containsSurfaceKeyword(text,
			"加到", "放到", "放进", "插入", "添加", "替换", "同步到", "铺到",
		) {
		return true
	}
	return strings.Contains(text, "字幕") &&
		containsSurfaceKeyword(text, "添加", "新增", "生成", "删除", "修改", "编辑", "调整")
}

func requestsPreviewCheck(text string) bool {
	return containsSurfaceKeyword(text,
		"质检", "黑帧", "静帧", "静音", "响度", "解码",
		"render_preview 已完成", "preview_",
	)
}

func requestsRenderWorkflow(text string) bool {
	return containsSurfaceKeyword(text,
		"渲染", "生成预览", "可分享预览", "离线画质", "preview.generate",
	)
}

func requestsExplicitPreviewWorkflow(text string) bool {
	return containsSurfaceKeyword(text, "预览", "离线画质", "preview.generate")
}

func requestsUserFinalExport(text string) bool {
	return containsSurfaceKeyword(text,
		"导出", "下载", "最终成片", "最终视频", "渲染成片", "mp4",
	)
}

func requestsUserFinalExportOnly(text string) bool {
	if !requestsUserFinalExport(text) {
		return false
	}
	return pendingEditingSurface(text) == 0 &&
		!requestsTimelineMutation(text) &&
		!requestsTalkingHeadWorkflow(text) &&
		!requestsBeatEditWorkflow(text) &&
		!requestsAssetSearchForTimelineEdit(text) &&
		!requestsExplicitPreviewWorkflow(text) &&
		!containsSurfaceKeyword(text, "预览质检")
}

func workflowToolCallSucceeded(message *schema.Message) bool {
	if message.ToolName == "asset.list_assets" {
		var result rushestools.AssetListResult
		return json.Unmarshal([]byte(message.Content), &result) == nil &&
			result.DraftID != "" && len(result.Assets) > 0
	}
	if message.ToolName == "shot.search" {
		var result struct {
			Shots []json.RawMessage `json:"shots"`
		}
		return json.Unmarshal([]byte(message.Content), &result) == nil && len(result.Shots) > 0
	}
	var result struct {
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(message.Content), &result) != nil {
		return false
	}
	return result.Status == "succeeded"
}

func requestsTalkingHeadWorkflow(text string) bool {
	return containsSurfaceKeyword(text,
		"口播", "台词", "气口", "重说", "转写", "逐字稿", "asr", "transcript", "speech.",
	)
}

func requestsBeatEditWorkflow(text string) bool {
	return containsSurfaceKeyword(text, "卡点", "拍点", "节拍", "音频", "bpm", "bgm", "beat")
}

func requestsReadOnlyMediaAnalysis(text string) bool {
	if requestsTimelineMutation(text) ||
		requestsMediaAnalysisEditWorkflow(text) ||
		requestsInitialTimelineComposition(text) ||
		requestsAssetSearchForTimelineEdit(text) ||
		requestsRenderWorkflow(text) ||
		requestsPreviewCheck(text) {
		return false
	}
	if !requestsBeatEditWorkflow(text) && !requestsTalkingHeadWorkflow(text) {
		return false
	}
	return containsSurfaceKeyword(text,
		"分析", "检测", "识别", "读取", "查看", "搜索", "检索", "转写", "逐字稿",
	)
}

func requestsMediaAnalysisEditWorkflow(text string) bool {
	if requestsTalkingHeadWorkflow(text) && containsSurfaceKeyword(text,
		"清理气口", "清理口播", "清理重说", "处理口播", "删除气口", "去掉气口", "口播重剪",
	) {
		return true
	}
	return requestsBeatEditWorkflow(text) && containsSurfaceKeyword(text,
		"做卡点", "做成卡点", "进行卡点", "卡点剪辑", "踩点剪辑", "按拍点剪", "按节拍剪", "节拍重剪",
	)
}

func requestsOnlyBeatAnalysis(text string) bool {
	return requestsBeatEditWorkflow(text) &&
		!requestsTalkingHeadWorkflow(text) &&
		containsSurfaceKeyword(text, "分析", "检测", "识别") &&
		!requestsTimelineInspect(text) &&
		!requestsTimelineCheckIntent(text)
}

func requestsAssetSearchForTimelineEdit(text string) bool {
	hasTimelineEdit := containsSurfaceKeyword(text,
		"时间线", "剪辑", "插入", "替换", "添加", "补一个", "clip", "patch",
	)
	explicitSearch := requestsShotSearch(text) || containsSurfaceKeyword(text, "找一个", "asset.", "shot")
	semanticInsert := containsSurfaceKeyword(text,
		"插入", "替换", "添加", "补一个",
	) && containsSurfaceKeyword(text, "镜头", "素材")
	return hasTimelineEdit && (explicitSearch || semanticInsert)
}

func requestsShotSearch(text string) bool {
	if containsSurfaceKeyword(text, "搜索镜头", "查找镜头", "检索镜头", "镜头检索", "找镜头") {
		return true
	}
	return strings.Contains(text, "检索") &&
		containsSurfaceKeyword(text, "镜头", "素材", "b-roll", "broll")
}

func containsSurfaceKeyword(text string, keywords ...string) bool {
	for _, keyword := range keywords {
		if strings.Contains(text, keyword) {
			return true
		}
	}
	return false
}

func modelToolSurfaceInterceptor(
	ctx context.Context,
	spec rushestools.Spec,
	_ any,
) error {
	session := modelToolSurfaceSessionFromContext(ctx)
	allowed, configured := session.allows(spec.Name)
	if !configured || allowed {
		return nil
	}
	return &rushestools.InterceptorRejection{
		Observation: "该工具不在本次模型调用按当前状态披露的工具面中，不能绕过动态能力边界执行。",
		Data: map[string]any{
			"error_code":      string(rushestools.ErrCodeToolNotInSurface),
			"tool":            spec.Name,
			"available_tools": session.boundNames(),
			"recovery":        "只调用 available_tools 中的工具；若需要另一阶段能力，先完成当前原子步骤并让模型在下一轮按最新状态重新绑定。",
		},
	}
}
