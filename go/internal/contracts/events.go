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
	Mode       VersionMode
	MergeKeys  []string
	DraftScope bool
}

// EventRegistry 是 Go 精简核心与前端仍在使用的生命周期契约。
var EventRegistry = map[string]EventSpec{
	"DraftCreated":                   {Mode: VersionMerge, MergeKeys: []string{"draft_id"}, DraftScope: true},
	"DraftRenamed":                   {Mode: VersionMerge, MergeKeys: []string{"draft_id", "name"}, DraftScope: true},
	"DraftCopied":                    {Mode: VersionMerge, MergeKeys: []string{"draft_id"}, DraftScope: true},
	"DraftTrashed":                   {Mode: VersionMerge, MergeKeys: []string{"draft_id"}, DraftScope: true},
	"AssetImported":                  {Mode: VersionMerge, MergeKeys: []string{"asset_id", "job_id"}},
	"AssetProbed":                    {Mode: VersionMerge, MergeKeys: []string{"asset_id"}},
	"AssetLinked":                    {Mode: VersionMerge, MergeKeys: []string{"draft_id", "asset_id"}, DraftScope: true},
	"AssetUnlinked":                  {Mode: VersionMerge, MergeKeys: []string{"draft_id", "asset_id"}, DraftScope: true},
	"MaterialUnderstandingStarted":   {Mode: VersionMerge, MergeKeys: []string{"asset_id"}},
	"MaterialUnderstandingCompleted": {Mode: VersionMerge, MergeKeys: []string{"asset_id"}},
	"MaterialUnderstandingFailed":    {Mode: VersionMerge, MergeKeys: []string{"asset_id"}},
	"DecisionCreated":                {Mode: VersionStrict, MergeKeys: []string{"decision_id"}, DraftScope: true},
	"DecisionAnswered":               {Mode: VersionStrict, MergeKeys: []string{"decision_id"}, DraftScope: true},
	"ConversationContextCleared":     {Mode: VersionStrict, DraftScope: true},
	"TimelineVersionCreated":         {Mode: VersionStrict, DraftScope: true},
	"TimelineValidated":              {Mode: VersionStrict, DraftScope: true},
	"TimelineValidationFailed":       {Mode: VersionStrict, DraftScope: true},
	"PreviewRendered":                {Mode: VersionMerge, MergeKeys: []string{"timeline_version", "artifact_id"}, DraftScope: true},
	"ExportCompleted":                {Mode: VersionMerge, MergeKeys: []string{"timeline_version", "artifact_id"}, DraftScope: true},
	"JobEnqueued":                    {Mode: VersionMerge, MergeKeys: []string{"job_id"}},
	"JobSucceeded":                   {Mode: VersionMerge, MergeKeys: []string{"job_id"}},
	"JobFailed":                      {Mode: VersionMerge, MergeKeys: []string{"job_id"}},
	"JobCancelled":                   {Mode: VersionMerge, MergeKeys: []string{"job_id"}},
	"ProxyGenerated":                 {Mode: VersionMerge, MergeKeys: []string{"asset_id", "proxy_object_hash"}},
	"JobProgress":                    {Mode: VersionMerge, MergeKeys: []string{"job_id", "progress"}},
	"PreviewViewed":                  {Mode: VersionMerge, MergeKeys: []string{"preview_id"}, DraftScope: true},
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
	if !ok {
		return EventSpec{}, false
	}
	// Workspace decisions use merge semantics; draft decisions remain strict.
	if (event.Type == "DecisionCreated" || event.Type == "DecisionAnswered") &&
		stringValue(event.Payload["scope_type"]) == "workspace" {
		spec.Mode = VersionMerge
	}
	return spec, true
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
	if spec.Mode != VersionMerge || len(spec.MergeKeys) == 0 {
		return "", nil
	}
	parts := make([]string, 0, len(spec.MergeKeys))
	for _, key := range spec.MergeKeys {
		parts = append(parts, key+"="+valueForKey(event, key))
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
	switch event.Type {
	case "DraftCreated", "DraftRenamed", "DraftCopied", "DraftTrashed",
		"AssetLinked", "AssetUnlinked", "AssetImported", "AssetProbed", "ProxyGenerated",
		"MaterialUnderstandingStarted", "MaterialUnderstandingCompleted", "MaterialUnderstandingFailed",
		"JobEnqueued", "JobProgress", "JobSucceeded", "JobFailed", "JobCancelled":
		return true
	default:
		return false
	}
}

func RoutesToDraft(event Event, draftID string) bool {
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
