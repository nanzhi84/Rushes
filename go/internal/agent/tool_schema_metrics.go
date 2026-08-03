package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

var logModelToolCatalogOnce sync.Once

// recordModelToolCatalog 记录 Registry 全量 LLM Catalog；它与模型是否配置、建图是否
// 成功无关。names 只来自注册表，不含用户素材、参数或密钥，可安全进入结构化日志。
func recordModelToolCatalog(ctx context.Context, registry *rushestools.Registry) {
	if registry == nil {
		return
	}
	catalog, err := modelToolSchemaSize(ctx, registry)
	if err != nil {
		slog.Warn("模型工具目录 schema 统计失败", "error", err)
		return
	}
	metricModelToolCatalogCount.Set(int64(len(catalog.PerToolRunes)))
	metricModelToolCatalogSchemaRunes.Set(int64(catalog.TotalRunes))
	metricModelToolCatalogSchemaBytes.Set(int64(catalog.TotalBytes))
	if entries, err := registry.ModelActionCatalog(); err == nil {
		if encoded, marshalErr := json.Marshal(entries); marshalErr == nil {
			metricModelActionCatalogRunes.Set(int64(len([]rune(string(encoded)))))
			metricModelActionCatalogBytes.Set(int64(len(encoded)))
		}
	}
	logModelToolCatalogOnce.Do(func() {
		slog.Info(
			"模型工具目录已加载",
			"catalog_tool_names", catalog.Names,
			"catalog_tool_count", len(catalog.PerToolRunes),
			"catalog_schema_runes", catalog.TotalRunes,
		)
	})
}

// recordBoundModelActionSchemas 记录每次 provider 实际收到的 schema 并集；
// 它由 transcript 中成功的 tool.load 回执唯一确定。
func recordBoundModelActionSchemas(ctx context.Context, boundTools []tool.BaseTool) {
	bound, err := modelToolSchemaSizeFromTools(ctx, boundTools)
	if err != nil {
		slog.Warn("模型实际绑定工具 schema 统计失败", "error", err)
		return
	}
	metricModelToolBoundCount.Observe(int64(len(bound.PerToolRunes)))
	metricModelToolBoundSchemaRunes.Observe(int64(bound.TotalRunes))
	metricModelToolBoundSchemaBytes.Observe(int64(bound.TotalBytes))
	slog.Info(
		"模型 action schema 已绑定",
		"bound_tool_names", bound.Names,
		"bound_tool_count", len(bound.PerToolRunes),
		"bound_schema_runes", bound.TotalRunes,
		"bound_schema_bytes", bound.TotalBytes,
	)
}

type modelToolSchemaMetrics struct {
	TotalRunes   int
	TotalBytes   int
	PerToolRunes map[string]int
	Names        []string
}

type modelToolSchemaPayload struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

func modelToolSchemaSize(ctx context.Context, registry *rushestools.Registry) (modelToolSchemaMetrics, error) {
	tools := make([]tool.BaseTool, 0, len(registry.Specs(true)))
	for _, spec := range registry.Specs(true) {
		if spec.Exposure != rushestools.ExposureLLM {
			continue
		}
		tools = append(tools, spec.Implementation)
	}
	return modelToolSchemaSizeFromTools(ctx, tools)
}

func modelToolSchemaSizeFromTools(
	ctx context.Context,
	tools []tool.BaseTool,
) (modelToolSchemaMetrics, error) {
	metrics := modelToolSchemaMetrics{PerToolRunes: map[string]int{}}
	for _, implementation := range tools {
		info, err := implementation.Info(ctx)
		if err != nil {
			return modelToolSchemaMetrics{}, fmt.Errorf("读取工具信息: %w", err)
		}
		schema, err := info.ToJSONSchema()
		if err != nil {
			return modelToolSchemaMetrics{}, fmt.Errorf("%s schema: %w", info.Name, err)
		}
		encoded, err := json.Marshal(modelToolSchemaPayload{
			Name: info.Name, Description: info.Desc, Parameters: schema,
		})
		if err != nil {
			return modelToolSchemaMetrics{}, fmt.Errorf("%s payload: %w", info.Name, err)
		}
		runes := utf8.RuneCount(encoded)
		if _, exists := metrics.PerToolRunes[info.Name]; exists {
			return modelToolSchemaMetrics{}, fmt.Errorf("实际绑定工具重复: %s", info.Name)
		}
		metrics.PerToolRunes[info.Name] = runes
		metrics.Names = append(metrics.Names, info.Name)
		metrics.TotalRunes += runes
		metrics.TotalBytes += len(encoded)
	}
	sort.Strings(metrics.Names)
	return metrics, nil
}
