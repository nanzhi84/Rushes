package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

var logModelToolSchemaSizeOnce sync.Once

func recordModelToolSchemaSize(ctx context.Context, registry *rushestools.Registry) {
	if registry == nil {
		return
	}
	logModelToolSchemaSizeOnce.Do(func() {
		bytes, count := modelToolSchemaSize(ctx, registry)
		slog.Info("模型工具 schema 已加载", "tool_count", count, "schema_bytes", bytes)
	})
}

func modelToolSchemaSize(ctx context.Context, registry *rushestools.Registry) (int, int) {
	total := 0
	count := 0
	for _, spec := range registry.Specs(true) {
		if spec.Exposure != rushestools.ExposureLLM {
			continue
		}
		info, err := spec.Implementation.Info(ctx)
		if err != nil {
			continue
		}
		schema, err := info.ToJSONSchema()
		if err != nil {
			continue
		}
		encoded, err := json.Marshal(schema)
		if err != nil {
			continue
		}
		total += len(encoded)
		count++
	}
	return total, count
}
