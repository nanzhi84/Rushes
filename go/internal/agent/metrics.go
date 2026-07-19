package agent

import (
	"context"
	"log/slog"

	"github.com/nanzhi84/Rushes/go/internal/telemetry"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// 本文件集中声明 agent 域的进程级度量（#95 H3）。全部经 telemetry→expvar 暴露到
// /debug/metrics，不入 OpenAPI 冻结面。命名统一 agent_ 前缀，避免跨域 expvar 撞名。
var (
	// 回合时长（毫秒）与结局分类。
	metricTurnDurationMS = telemetry.NewHistogram(
		"agent_turn_duration_ms", []int64{100, 500, 1000, 5000, 15000, 60000},
	)
	metricTurnFinished  = telemetry.NewCounter("agent_turn_finished_total")
	metricTurnFailed    = telemetry.NewCounter("agent_turn_failed_total")
	metricTurnCancelled = telemetry.NewCounter("agent_turn_cancelled_total")

	// 模型调用延迟（毫秒）、重试数与重试原因分类（H1a 上下文超限 vs 超时）。
	metricModelCallMS = telemetry.NewHistogram(
		"agent_model_call_ms", []int64{200, 500, 1000, 3000, 10000, 30000},
	)
	metricModelRetriesTotal       = telemetry.NewCounter("agent_model_retries_total")
	metricModelRetryContextLength = telemetry.NewCounter("agent_model_retry_context_length_total")
	metricModelRetryTimeout       = telemetry.NewCounter("agent_model_retry_timeout_total")

	// 压缩触发/降级；触发时的历史 token 量（供阈值校准，H-B P2）。
	metricCompactionTriggered     = telemetry.NewCounter("agent_context_compaction_triggered_total")
	metricCompactionDegraded      = telemetry.NewCounter("agent_context_compaction_degraded_total")
	metricCompactionTriggerTokens = telemetry.NewHistogram(
		"agent_context_compaction_trigger_tokens", []int64{2000, 4000, 8000, 16000, 32000, 64000},
	)

	// 上下文四段 token：reference（世界态基线）/patch（RFC-7396 增量）/summary（压缩交接）/history。
	metricContextReferenceTokens = telemetry.NewHistogram("agent_context_reference_tokens", contextTokenBounds)
	metricContextPatchTokens     = telemetry.NewHistogram("agent_context_patch_tokens", contextTokenBounds)
	metricContextSummaryTokens   = telemetry.NewHistogram("agent_context_summary_tokens", contextTokenBounds)
	metricContextHistoryTokens   = telemetry.NewHistogram("agent_context_history_tokens", contextTokenBounds)

	// TurnQueue 深度（全 draft 累计的 pending+running 槽位）。
	metricTurnQueueDepth = telemetry.NewGauge("agent_turn_queue_depth")

	// 累计 prompt / cached prompt token，命中率由 PublishRatio 派生。
	metricPromptTokensTotal       = telemetry.NewCounter("agent_prompt_tokens_total")
	metricCachedPromptTokensTotal = telemetry.NewCounter("agent_cached_prompt_tokens_total")

	// 终态直通后仍到达的迟到 tool_call（H5，按 tool-call 去重后计数，H-B P2）。
	metricPassthroughLateToolCalls = telemetry.NewCounter("agent_passthrough_late_tool_calls_total")
	// 终态回复被反思质检重述的次数（H7）。
	metricReflectionRestated = telemetry.NewCounter("agent_reflection_restated_total")

	// 工具修复预算穷尽分因（H4）：streak = 连续失败链超限；cumulative = 连击预算被成功清零、
	// 但 turn 级累计预算仍超限——正是「交替 fail→success 想刷新预算却被累计计数挡住」的信号
	// （H-B P2「预算重叠」）。
	metricRecoveryStreakExhausted     = telemetry.NewCounter("agent_recovery_streak_exhausted_total")
	metricRecoveryCumulativeExhausted = telemetry.NewCounter("agent_recovery_cumulative_exhausted_total")

	// 工作区用户记忆注入规模（M6/M8）：三个累计条数、实际 section rune 分布与发生
	// 截断的构建次数。omitted ratio 由累计 omitted/total 派生，供容量继续校准。
	metricUserMemoryTotal    = telemetry.NewCounter("agent_user_memory_total")
	metricUserMemoryInjected = telemetry.NewCounter("agent_user_memory_injected")
	metricUserMemoryOmitted  = telemetry.NewCounter("agent_user_memory_omitted")
	metricUserMemoryRunes    = telemetry.NewHistogram(
		"agent_user_memory_section_runes", []int64{256, 512, 1000, 2000, 3000, 4000, 5000},
	)
	metricUserMemoryTruncated = telemetry.NewCounter("agent_user_memory_truncated")

	// 工具结果体量软护栏（T6）：完整结果仍回灌模型，只告警并计数；字节直方图供
	// 观察真实分布后决定是否需要硬截断以及按工具分档。
	metricToolResultBytes = telemetry.NewHistogram(
		"agent_tool_result_bytes", []int64{1024, 4096, 16384, 65536, 131072, 524288},
	)
	metricToolResultOversize = telemetry.NewCounter("tool_result_oversize_total")
)

// observeModelCall 记录一次模型调用延迟：进直方图度量 + 一条结构化日志（AC1「每次模型调用
// 延迟」的结构化记录）。draft_id 尽力从 ctx 取，取不到留空。
func observeModelCall(ctx context.Context, latencyMS int64) {
	metricModelCallMS.Observe(latencyMS)
	draftID, _ := rushestools.DraftID(ctx)
	slog.Info("model_call", "draft_id", draftID, "latency_ms", latencyMS)
}

// contextTokenBounds 是上下文四段 token 直方图的共用桶上界。
var contextTokenBounds = []int64{500, 2000, 8000, 32000, 128000}

func init() {
	// 缓存 prompt token 命中率 = 累计 cached / 累计 prompt。
	telemetry.PublishRatio("agent_cached_prompt_hit_ratio", func() float64 {
		prompt := metricPromptTokensTotal.Value()
		if prompt == 0 {
			return 0
		}
		return float64(metricCachedPromptTokensTotal.Value()) / float64(prompt)
	})
	telemetry.PublishRatio("agent_user_memory_omitted_ratio", func() float64 {
		total := metricUserMemoryTotal.Value()
		if total == 0 {
			return 0
		}
		return float64(metricUserMemoryOmitted.Value()) / float64(total)
	})
}

func recordUserMemoryInjection(total, included, sectionRunes int) {
	metricUserMemoryTotal.Add(int64(total))
	metricUserMemoryInjected.Add(int64(included))
	omitted := max(0, total-included)
	metricUserMemoryOmitted.Add(int64(omitted))
	metricUserMemoryRunes.Observe(int64(sectionRunes))
	if omitted > 0 {
		metricUserMemoryTruncated.Inc()
	}
}

// recordTurnOutcome 按结局把回合计入对应计数器。
func recordTurnOutcome(outcome string) {
	switch outcome {
	case "finished":
		metricTurnFinished.Inc()
	case "failed":
		metricTurnFailed.Inc()
	case "cancelled":
		metricTurnCancelled.Inc()
	}
}

// recordContextSegmentTokens 把一次上下文组装的四段 token 记入各自直方图。
func recordContextSegmentTokens(reference, patch, summary, history int) {
	metricContextReferenceTokens.Observe(int64(reference))
	metricContextPatchTokens.Observe(int64(patch))
	metricContextSummaryTokens.Observe(int64(summary))
	metricContextHistoryTokens.Observe(int64(history))
}
