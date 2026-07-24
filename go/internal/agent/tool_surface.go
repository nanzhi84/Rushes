package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
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
	bound, err := surface.bind(ctx, messages)
	if err != nil {
		return nil, err
	}
	return bound.Generate(ctx, messages, options...)
}

func (surface *dynamicToolSurfaceModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	bound, err := surface.bind(ctx, messages)
	if err != nil {
		return nil, err
	}
	return bound.Stream(ctx, messages, options...)
}

func (surface *dynamicToolSurfaceModel) bind(
	ctx context.Context,
	messages []*schema.Message,
) (model.ToolCallingChatModel, error) {
	specs, err := selectModelToolSurface(ctx, surface.registry, messages)
	if err != nil {
		return nil, err
	}
	implementations := implementationsForSpecs(specs)
	infos := make([]*schema.ToolInfo, 0, len(implementations))
	for _, implementation := range implementations {
		info, infoErr := implementation.Info(ctx)
		if infoErr != nil {
			return nil, fmt.Errorf("读取动态工具信息: %w", infoErr)
		}
		infos = append(infos, info)
	}
	bound, err := surface.inner.WithTools(infos)
	if err != nil {
		return nil, err
	}
	recordBoundModelToolSurface(ctx, implementations)
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	if session := modelToolSurfaceSessionFromContext(ctx); session != nil {
		session.set(names)
	}
	return bound, nil
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
	lane := inferModelToolSurface(allowed, messages)
	lane = surfaceWithAvailablePrerequisites(allowed, lane, latestUserSurfaceText(messages))
	selected := filterSurface(allowed, lane)
	if lane == rushestools.SurfaceTimelineEdit &&
		requestsTalkingHeadWorkflow(latestUserSurfaceText(messages)) {
		// 口播删剪后仍需重新观察 source→timeline 映射和按保留台词检索 B-roll。
		// 这些证据工具与原子编辑共享当前轮工具面，但不会把旧复合编辑带回来。
		selected = append(selected, filterSpecsByName(
			allowed,
			"media.detect_shots",
			"shot.search",
			"speech.search",
		)...)
	}
	if lane == rushestools.SurfaceTimelineEdit && requestsTimelineInspect(latestUserSurfaceText(messages)) {
		selected = filterSpecsByName(selected, "timeline.inspect")
	}
	if len(selected) == 0 && lane != rushestools.SurfaceDiscovery {
		lane = rushestools.SurfaceDiscovery
		selected = filterSurface(allowed, lane)
	}
	if len(selected) == 0 {
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

func latestUserSurfaceText(messages []*schema.Message) string {
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message == nil || message.Role != schema.User {
			continue
		}
		content := strings.TrimSpace(message.Content)
		if content != "" && !isAutomaticContinuationSurfaceMessage(content) {
			return strings.ToLower(content)
		}
	}
	return ""
}

func isAutomaticContinuationSurfaceMessage(content string) bool {
	return strings.Contains(content, "这是原任务的自动续跑，不是新的用户请求。") ||
		strings.Contains(content, "这是同一条任务的继续，不是新的请求。")
}

func inferModelToolSurface(
	specs []rushestools.Spec,
	messages []*schema.Message,
) rushestools.Surface {
	text := latestUserSurfaceText(messages)
	if needsPlanUpdateSurface(messages) {
		return rushestools.SurfaceControl
	}
	if recent := recentSuccessfulWorkflowSurface(messages, text); recent != 0 {
		return recent
	}
	if latestUserIsAutomaticContinuation(messages) {
		if requestsPreviewCheck(text) && hasAllowedTool(specs, "preview.check") {
			return rushestools.SurfacePreviewCheck
		}
		if requestsRenderWorkflow(text) &&
			hasAnyAllowedTool(specs, "timeline.check", "render.start", "job.read") {
			return rushestools.SurfaceRender
		}
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
	// preview；只有新 job.read 返回成功产物后，recentSuccessfulWorkflowSurface
	// 才会把后续轮次推进 PreviewCheck。
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
		"搜索镜头", "查找镜头", "找镜头", "asset.", "shot"):
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

func latestUserIsAutomaticContinuation(messages []*schema.Message) bool {
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message != nil && message.Role == schema.User {
			return isAutomaticContinuationSurfaceMessage(message.Content)
		}
	}
	return false
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
			if isAutomaticContinuationSurfaceMessage(message.Content) {
				continue
			}
			return false
		}
		if message.Role == schema.Tool && message.ToolName == toolName {
			return workflowToolCallSucceeded(message)
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
			"speech.transcribe",
			"speech.search",
			"audio.analyze_speech_pauses",
			"shot.search",
			"timeline.insert",
			"timeline.inspect",
		) {
			return rushestools.SurfaceDiscovery
		}
	case rushestools.SurfaceBeatEdit:
		if !hasAnyAllowedTool(specs,
			"audio.analyze_beats",
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
			!strings.Contains(text, "timeline.insert") {
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
		if !hasAllowedTool(specs, "timeline.check") &&
			!hasAllowedTool(specs, "render.start") &&
			(!hasAllowedTool(specs, "job.read") || !strings.Contains(text, "job.read")) {
			return rushestools.SurfaceDiscovery
		}
	case rushestools.SurfacePreviewCheck:
		if !hasAllowedTool(specs, "preview.check") {
			if hasAllowedTool(specs, "timeline.check") ||
				hasAllowedTool(specs, "render.start") {
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
	seen := make(map[string]struct{})
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message == nil {
			continue
		}
		if message.Role == schema.User {
			if isAutomaticContinuationSurfaceMessage(message.Content) {
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
		case "plan.update", "memory.update":
			return remainingWorkflowSurface(userText)
		case "media.detect_shots", "speech.transcribe":
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
		case "timeline.check":
			return rushestools.SurfaceRender
		case "render.start":
			return rushestools.SurfaceRender
		case "job.read":
			if requestsPreviewCheck(userText) && completedPreviewJob(message.Content) {
				return rushestools.SurfacePreviewCheck
			}
			return rushestools.SurfaceRender
		}
	}
	return 0
}

func isWorkflowTransitionTool(name string) bool {
	switch name {
	case "plan.update",
		"memory.update",
		"media.detect_shots",
		"speech.transcribe",
		"speech.search",
		"shot.search",
		"timeline.insert",
		"timeline.delete",
		"timeline.update",
		"timeline.split",
		"timeline.check",
		"render.start",
		"job.read":
		return true
	default:
		return false
	}
}

func remainingWorkflowSurface(text string) rushestools.Surface {
	switch {
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
	case containsSurfaceKeyword(text,
		"组装初版时间线", "建立时间线", "创建时间线", "初版时间线", "首剪"):
		return rushestools.SurfaceDiscovery
	default:
		return 0
	}
}

func pendingEditingSurface(text string) rushestools.Surface {
	switch {
	case requestsTalkingHeadWorkflow(text) &&
		containsSurfaceKeyword(text,
			"清理", "剪辑", "剪掉", "删除", "去掉", "修剪", "编辑", "修改", "处理"):
		return rushestools.SurfaceTalkingHead
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
	return strings.Contains(text, "字幕") &&
		containsSurfaceKeyword(text, "添加", "新增", "生成", "删除", "修改", "编辑", "调整")
}

func requestsPreviewCheck(text string) bool {
	return containsSurfaceKeyword(text,
		"质检", "黑帧", "静帧", "静音", "响度", "解码", "inspect_preview",
		"render_preview 已完成", "preview_",
	)
}

func requestsRenderWorkflow(text string) bool {
	return containsSurfaceKeyword(text,
		"渲染", "导出", "最终成片", "mp4", "render.start", "job.read",
	)
}

func workflowToolCallSucceeded(message *schema.Message) bool {
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
	return result.Status == "succeeded" ||
		(message.ToolName == "render.start" && result.Status == "queued")
}

func completedPreviewJob(content string) bool {
	var result struct {
		Data struct {
			Kind      string `json:"kind"`
			JobStatus string `json:"job_status"`
		} `json:"data"`
	}
	return json.Unmarshal([]byte(content), &result) == nil &&
		result.Data.Kind == "render_preview" &&
		result.Data.JobStatus == "succeeded"
}

func requestsTalkingHeadWorkflow(text string) bool {
	return containsSurfaceKeyword(text,
		"口播", "台词", "气口", "重说", "转写", "逐字稿", "asr", "transcript", "speech.",
	)
}

func requestsBeatEditWorkflow(text string) bool {
	return containsSurfaceKeyword(text, "卡点", "拍点", "节拍", "音频", "bpm", "bgm", "beat")
}

func requestsAssetSearchForTimelineEdit(text string) bool {
	hasTimelineEdit := containsSurfaceKeyword(text,
		"时间线", "剪辑", "插入", "替换", "添加", "补一个", "clip", "patch",
	)
	explicitSearch := containsSurfaceKeyword(text,
		"搜索镜头", "查找镜头", "找镜头", "找一个", "asset.", "shot",
	)
	semanticInsert := containsSurfaceKeyword(text,
		"插入", "替换", "添加", "补一个",
	) && containsSurfaceKeyword(text, "镜头", "素材")
	return hasTimelineEdit && (explicitSearch || semanticInsert)
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
