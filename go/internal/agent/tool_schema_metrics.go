package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"unicode/utf8"

	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

var logModelToolSchemaSizeOnce sync.Once

func recordModelToolSchemaSize(ctx context.Context, registry *rushestools.Registry) {
	if registry == nil {
		return
	}
	logModelToolSchemaSizeOnce.Do(func() {
		metrics, err := modelToolSchemaSize(ctx, registry)
		if err != nil {
			slog.Warn("模型工具 schema 统计失败", "error", err)
			return
		}
		slog.Info(
			"模型工具 schema 已加载",
			"tool_count", len(metrics.PerToolRunes),
			"schema_runes", metrics.TotalRunes,
		)
	})
}

type modelToolSchemaMetrics struct {
	TotalRunes   int
	PerToolRunes map[string]int
}

type modelToolSchemaPayload struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

func modelToolSchemaSize(ctx context.Context, registry *rushestools.Registry) (modelToolSchemaMetrics, error) {
	metrics := modelToolSchemaMetrics{PerToolRunes: map[string]int{}}
	for _, spec := range registry.Specs(true) {
		if spec.Exposure != rushestools.ExposureLLM {
			continue
		}
		info, err := spec.Implementation.Info(ctx)
		if err != nil {
			return modelToolSchemaMetrics{}, fmt.Errorf("%s info: %w", spec.Name, err)
		}
		schema, err := info.ToJSONSchema()
		if err != nil {
			return modelToolSchemaMetrics{}, fmt.Errorf("%s schema: %w", spec.Name, err)
		}
		encoded, err := json.Marshal(modelToolSchemaPayload{
			Name: spec.Name, Description: spec.Description, Parameters: schema,
		})
		if err != nil {
			return modelToolSchemaMetrics{}, fmt.Errorf("%s payload: %w", spec.Name, err)
		}
		runes := utf8.RuneCount(encoded)
		metrics.PerToolRunes[spec.Name] = runes
		metrics.TotalRunes += runes
	}
	return metrics, nil
}
