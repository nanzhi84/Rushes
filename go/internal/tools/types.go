package tools

import (
	"context"
	"errors"
)

type contextKey string

const (
	draftIDKey  contextKey = "rushes_draft_id"
	reporterKey contextKey = "rushes_tool_reporter"
)

type Executor interface {
	ExecuteTool(context.Context, string, any) (any, error)
}

type Reporter func(name, phase string, input, output any, err error)

func WithDraftID(ctx context.Context, draftID string) context.Context {
	return context.WithValue(ctx, draftIDKey, draftID)
}

func DraftID(ctx context.Context) (string, error) {
	value, _ := ctx.Value(draftIDKey).(string)
	if value == "" {
		return "", errors.New("工具执行缺少 active draft")
	}
	return value, nil
}

func WithReporter(ctx context.Context, reporter Reporter) context.Context {
	return context.WithValue(ctx, reporterKey, reporter)
}

type ToolResult struct {
	Status      string         `json:"status"`
	Observation string         `json:"observation"`
	Data        map[string]any `json:"data,omitempty"`
}

type AssetImportInput struct {
	Path        string `json:"path" jsonschema:"required" jsonschema_description:"已由用户在文件选择器确认的本地路径"`
	StorageMode string `json:"storage_mode,omitempty" jsonschema_description:"reference 或 copy"`
	Kind        string `json:"kind,omitempty" jsonschema_description:"video audio image font"`
}

type AssetListInput struct {
	Kind       string `json:"kind,omitempty"`
	OnlyUsable *bool  `json:"only_usable,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	After      string `json:"after,omitempty"`
}

type AssetManifest struct {
	AssetID             string  `json:"asset_id"`
	Filename            string  `json:"filename"`
	Kind                string  `json:"kind"`
	DurationSec         float64 `json:"duration_sec,omitempty"`
	Usable              bool    `json:"usable"`
	IngestStatus        string  `json:"ingest_status"`
	UnderstandingStatus string  `json:"understanding_status"`
}

type AssetListResult struct {
	DraftID   string          `json:"draft_id"`
	Assets    []AssetManifest `json:"assets"`
	Total     int             `json:"total"`
	NextAfter string          `json:"next_after,omitempty"`
}

type UnderstandInput struct {
	AssetIDs         []string `json:"asset_ids" jsonschema:"required"`
	Depth            string   `json:"depth,omitempty" jsonschema_description:"scan 或 deep"`
	Focus            string   `json:"focus,omitempty"`
	MaxStepsPerAsset int      `json:"max_steps_per_asset,omitempty"`
}

type UnderstandResult struct {
	DraftID  string   `json:"draft_id"`
	JobID    string   `json:"job_id"`
	AssetIDs []string `json:"asset_ids"`
	Status   string   `json:"status"`
}

type DecisionOptionInput struct {
	OptionID    string `json:"option_id" jsonschema:"required"`
	Label       string `json:"label" jsonschema:"required"`
	Description string `json:"description,omitempty"`
}

type AskUserInput struct {
	Question      string                `json:"question" jsonschema:"required"`
	Options       []DecisionOptionInput `json:"options,omitempty"`
	AllowFreeText *bool                 `json:"allow_free_text,omitempty"`
	Blocking      *bool                 `json:"blocking,omitempty"`
}

type DecisionAnswerInput struct {
	DecisionID string         `json:"decision_id" jsonschema:"required"`
	OptionID   string         `json:"option_id,omitempty"`
	FreeText   string         `json:"free_text,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
}

type ComposeClip struct {
	AssetID     string  `json:"asset_id" jsonschema:"required"`
	SourceStart float64 `json:"source_start_s"`
	SourceEnd   float64 `json:"source_end_s" jsonschema:"required"`
	Role        string  `json:"role" jsonschema:"required"`
}

type ComposeInitialInput struct {
	Clips []ComposeClip `json:"clips" jsonschema:"required"`
}

type TimelinePatchInput struct {
	Op map[string]any `json:"op" jsonschema:"required"`
}

type TimelineValidateInput struct{}

type TimelineInspectInput struct {
	Version int `json:"version,omitempty"`
}

type TimelineRestoreInput struct {
	SourceVersion int `json:"source_version" jsonschema:"required"`
}

type RenderPreviewInput struct{}

type RenderFinalInput struct{}

type RenderStatusInput struct{}

type RenderInspectInput struct {
	PreviewID string   `json:"preview_id" jsonschema:"required"`
	Checks    []string `json:"checks,omitempty"`
}

type PreviewInspectionResult struct {
	Summary  string                   `json:"summary"`
	Degraded bool                     `json:"degraded"`
	Issues   []map[string]interface{} `json:"issues"`
}

type ConfirmActionInput struct {
	Question  string         `json:"question" jsonschema:"required"`
	ToolName  string         `json:"tool_name" jsonschema:"required"`
	Arguments map[string]any `json:"arguments" jsonschema:"required"`
}
