package contracts

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type VersionMode string

const (
	VersionStrict VersionMode = "strict"
	VersionMerge  VersionMode = "merge"
)

type Actor string

const (
	ActorUser  Actor = "user"
	ActorAgent Actor = "agent"
	ActorJob   Actor = "job"
	// ActorSystem 现在没有任何发射方，但必须保留为受理值：旧 Python 后端写过
	// actor="system" 的事件，SSE 重放会对 event_log 里的历史行做 ParseEvent +
	// Validate，校验失败的行会被静默丢弃（见 api/sse.go）。删掉它等于让旧工作区
	// 丢失这部分历史。schemaV3 清理 TimelineVersionRestored 就是被同类问题咬过。
	ActorSystem Actor = "system"
)

type EventSpec struct {
	Mode               VersionMode
	WorkspaceScopeMode VersionMode
	MergeKeys          []string
	OptionalMergeKeys  []string
	DraftScope         bool
	Routes             EventRoutes
}

type EventRoutes uint8

const (
	RouteWorkspace EventRoutes = 1 << iota
	RouteDraft
)

func (routes EventRoutes) Includes(route EventRoutes) bool {
	return routes&route != 0
}

// EventRegistry 是 Go 精简核心与前端仍在使用的生命周期契约。
var EventRegistry = map[string]EventSpec{
	"DraftCreated":                   {Mode: VersionMerge, MergeKeys: []string{"draft_id"}, DraftScope: true, Routes: RouteWorkspace | RouteDraft},
	"DraftRenamed":                   {Mode: VersionMerge, MergeKeys: []string{"draft_id", "name"}, DraftScope: true, Routes: RouteWorkspace | RouteDraft},
	"DraftCopied":                    {Mode: VersionMerge, MergeKeys: []string{"draft_id"}, DraftScope: true, Routes: RouteWorkspace | RouteDraft},
	"DraftTrashed":                   {Mode: VersionMerge, MergeKeys: []string{"draft_id"}, DraftScope: true, Routes: RouteWorkspace | RouteDraft},
	"AssetImported":                  {Mode: VersionMerge, MergeKeys: []string{"asset_id", "job_id"}, Routes: RouteWorkspace | RouteDraft},
	"AssetProbed":                    {Mode: VersionMerge, MergeKeys: []string{"asset_id"}, Routes: RouteWorkspace | RouteDraft},
	"AssetLinked":                    {Mode: VersionMerge, MergeKeys: []string{"draft_id", "asset_id"}, DraftScope: true, Routes: RouteWorkspace | RouteDraft},
	"AssetUnlinked":                  {Mode: VersionMerge, MergeKeys: []string{"draft_id", "asset_id"}, DraftScope: true, Routes: RouteWorkspace | RouteDraft},
	"MaterialUnderstandingStarted":   {Mode: VersionMerge, MergeKeys: []string{"asset_id"}, OptionalMergeKeys: []string{"job_id", "attempt"}, Routes: RouteWorkspace | RouteDraft},
	"MaterialUnderstandingCompleted": {Mode: VersionMerge, MergeKeys: []string{"asset_id"}, OptionalMergeKeys: []string{"job_id", "attempt"}, Routes: RouteWorkspace | RouteDraft},
	"MaterialUnderstandingFailed":    {Mode: VersionMerge, MergeKeys: []string{"asset_id"}, OptionalMergeKeys: []string{"job_id", "attempt"}, Routes: RouteWorkspace | RouteDraft},
	"DecisionCreated":                {Mode: VersionStrict, WorkspaceScopeMode: VersionMerge, MergeKeys: []string{"decision_id"}, DraftScope: true, Routes: RouteDraft},
	"DecisionAnswered":               {Mode: VersionStrict, WorkspaceScopeMode: VersionMerge, MergeKeys: []string{"decision_id"}, DraftScope: true, Routes: RouteDraft},
	"ConversationContextCleared":     {Mode: VersionStrict, DraftScope: true, Routes: RouteDraft},
	"TimelineVersionCreated":         {Mode: VersionStrict, DraftScope: true, Routes: RouteDraft},
	"TimelineVersionRestored":        {Mode: VersionStrict, DraftScope: true, Routes: RouteDraft},
	"TimelineValidated":              {Mode: VersionStrict, DraftScope: true, Routes: RouteDraft},
	"TimelineValidationFailed":       {Mode: VersionStrict, DraftScope: true, Routes: RouteDraft},
	"PreviewRendered":                {Mode: VersionMerge, MergeKeys: []string{"timeline_version", "artifact_id"}, DraftScope: true, Routes: RouteDraft},
	"ExportCompleted":                {Mode: VersionMerge, MergeKeys: []string{"timeline_version", "artifact_id"}, DraftScope: true, Routes: RouteDraft},
	"JobEnqueued":                    {Mode: VersionMerge, MergeKeys: []string{"job_id"}, Routes: RouteWorkspace | RouteDraft},
	"JobSucceeded":                   {Mode: VersionMerge, MergeKeys: []string{"job_id"}, Routes: RouteWorkspace | RouteDraft},
	"JobFailed":                      {Mode: VersionMerge, MergeKeys: []string{"job_id"}, Routes: RouteWorkspace | RouteDraft},
	"JobCancelled":                   {Mode: VersionMerge, MergeKeys: []string{"job_id"}, Routes: RouteWorkspace | RouteDraft},
	"ProxyGenerated":                 {Mode: VersionMerge, MergeKeys: []string{"asset_id", "proxy_object_hash"}, Routes: RouteWorkspace | RouteDraft},
	"JobProgress":                    {Mode: VersionMerge, MergeKeys: []string{"job_id", "progress"}, OptionalMergeKeys: []string{"update_id"}, Routes: RouteWorkspace | RouteDraft},
	"PreviewViewed":                  {Mode: VersionMerge, MergeKeys: []string{"preview_id"}, DraftScope: true, Routes: RouteDraft},
}

type Event struct {
	Type        string         `json:"event"`
	Actor       Actor          `json:"actor"`
	DraftID     string         `json:"draft_id,omitempty"`
	BaseVersion *int           `json:"base_version,omitempty"`
	Payload     map[string]any `json:"payload"`
	CreatedAt   string         `json:"created_at,omitempty"`
}

func (event Event) Spec() (EventSpec, bool) {
	spec, ok := EventRegistry[event.Type]
	return spec, ok
}

// VersionMode returns the effective concurrency mode without mutating the
// registry declaration. Workspace-scoped variants opt into their mode in spec.
func (event Event) VersionMode() (VersionMode, bool) {
	spec, ok := event.Spec()
	if !ok {
		return "", false
	}
	if stringValue(event.Payload["scope_type"]) == "workspace" && spec.WorkspaceScopeMode != "" {
		return spec.WorkspaceScopeMode, true
	}
	return spec.Mode, true
}

func (event Event) Validate() error {
	spec, ok := event.Spec()
	if !ok {
		return fmt.Errorf("未知领域事件 %q", event.Type)
	}
	if !event.Actor.Valid() {
		return fmt.Errorf("事件 %s 的 actor 无效", event.Type)
	}
	if event.Payload == nil {
		return fmt.Errorf("事件 %s 缺少 payload", event.Type)
	}
	if spec.DraftScope && event.DraftID == "" && stringValue(event.Payload["scope_type"]) != "workspace" {
		return fmt.Errorf("事件 %s 缺少 draft_id", event.Type)
	}
	for _, key := range spec.MergeKeys {
		if valueForKey(event, key) == "" {
			return fmt.Errorf("事件 %s 缺少 merge key %s", event.Type, key)
		}
	}
	if event.Type == "TimelineVersionRestored" {
		mode := stringValue(event.Payload["mode"])
		if stringValue(event.Payload["checkpoint_id"]) == "" ||
			stringValue(event.Payload["restore_checkpoint_id"]) == "" {
			return errors.New("事件 TimelineVersionRestored 缺少 checkpoint_id")
		}
		if mode != "timeline" && mode != "conversation" && mode != "both" {
			return errors.New("事件 TimelineVersionRestored mode 无效")
		}
		if mode != "conversation" && stringValue(event.Payload["timeline_version"]) == "" {
			return errors.New("事件 TimelineVersionRestored 缺少 timeline_version")
		}
	}
	return nil
}

func (actor Actor) Valid() bool {
	switch actor {
	case ActorUser, ActorAgent, ActorJob, ActorSystem:
		return true
	default:
		return false
	}
}

func (event Event) MergeKey() (string, error) {
	spec, ok := event.Spec()
	if !ok {
		return "", errors.New("事件未注册")
	}
	mode, _ := event.VersionMode()
	if mode != VersionMerge || len(spec.MergeKeys) == 0 {
		return "", nil
	}
	parts := make([]string, 0, len(spec.MergeKeys)+len(spec.OptionalMergeKeys))
	for _, key := range spec.MergeKeys {
		parts = append(parts, key+"="+valueForKey(event, key))
	}
	for _, key := range spec.OptionalMergeKeys {
		if value := valueForKey(event, key); value != "" {
			parts = append(parts, key+"="+value)
		}
	}
	return strings.Join(parts, "\x1f"), nil
}

func (event Event) JSON() ([]byte, error) {
	return json.Marshal(event)
}

func ParseEvent(data []byte) (Event, error) {
	var event Event
	if err := json.Unmarshal(data, &event); err != nil {
		return Event{}, err
	}
	return event, event.Validate()
}

func RoutesToWorkspace(event Event) bool {
	spec, ok := event.Spec()
	return ok && spec.Routes.Includes(RouteWorkspace)
}

func RoutesToDraft(event Event, draftID string) bool {
	spec, ok := event.Spec()
	if !ok || !spec.Routes.Includes(RouteDraft) {
		return false
	}
	if event.DraftID == draftID {
		return true
	}
	return stringValue(event.Payload["requested_by_draft_id"]) == draftID
}

func valueForKey(event Event, key string) string {
	if key == "draft_id" {
		return event.DraftID
	}
	return stringValue(event.Payload[key])
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return fmt.Sprintf("%g", typed)
	case float32:
		return fmt.Sprintf("%g", typed)
	case int:
		return fmt.Sprintf("%d", typed)
	case int64:
		return fmt.Sprintf("%d", typed)
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

// TurnStreamSubagentProgress 是子过程进度 SSE 事件类型,agent(编排)与
// agentexec(领域执行)共用,置于契约层单一事实源。
const TurnStreamSubagentProgress = "subagent_progress"
