package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/telemetry"
)

// plainReplyModel 是最简终态文本模型：一轮就吐可见正文收尾，用于验证一个完整回合的
// 结构化日志与度量（#95 H3 验收）。
type plainReplyModel struct{}

func (plainReplyModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return plainReplyModel{}, nil
}

func (plainReplyModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("好的，已处理", nil), nil
}

func (plainReplyModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	reader, writer := schema.Pipe[*schema.Message](1)
	writer.Send(schema.AssistantMessage("好的，已处理", nil), nil)
	writer.Close()
	return reader, nil
}

// TestTurnEmitsStructuredLogAndMetrics 覆盖 H3 两条验收：
// 1) 跑完一个完整回合后，日志文件含回合开始/结束的结构化记录；
// 2) /debug/metrics（telemetry.Handler）输出回合/模型等度量。
// 非并行：slog 默认器与包级度量都是进程级，串行才能精确断言增量与日志归属。
func TestTurnEmitsStructuredLogAndMetrics(t *testing.T) {
	logDir := t.TempDir()
	savedLogger := slog.Default()
	closer, err := telemetry.InstallJSONLogger(logDir, "agenttest", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer.Close(); slog.SetDefault(savedLogger) }()

	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_metrics")
	service, err := NewService(t.Context(), database, plainReplyModel{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	finishedBefore := metricTurnFinished.Value()

	_, stream, unsubscribe := service.Hub().Subscribe("draft_metrics")
	defer unsubscribe()
	const userUtterance = "把开头剪短一点 token=SENTINEL_SECRET_do_not_persist_9z8y7x"
	if !service.Queue().EnqueueUserMessage("draft_metrics", "user_1", userUtterance) {
		t.Fatal("enqueue failed")
	}
	service.Queue().JoinDraft("draft_metrics")

	var outcome any
	deadline := time.After(5 * time.Second)
	for outcome == nil {
		select {
		case event := <-stream:
			if event["type"] == TurnStreamTurnEnded {
				outcome = event["outcome"]
			}
		case <-deadline:
			t.Fatal("等待 turn_ended 超时")
		}
	}
	if outcome != "finished" {
		t.Fatalf("回合应正常收尾，实得 outcome=%v", outcome)
	}

	// 验收 2：度量增长。
	if metricTurnFinished.Value() <= finishedBefore {
		t.Fatalf("回合结局计数未增长: before=%d after=%d", finishedBefore, metricTurnFinished.Value())
	}
	if count, _, _, _ := metricModelCallMS.Snapshot(); count == 0 {
		t.Fatal("模型调用延迟直方图应有观测")
	}

	// 验收 2：/debug/metrics 输出核心度量名。
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/debug/metrics", nil)
	telemetry.Handler().ServeHTTP(recorder, request)
	body := recorder.Body.String()
	for _, name := range []string{
		"agent_turn_finished_total", "agent_model_call_ms", "agent_turn_duration_ms",
		"agent_context_history_tokens", "agent_cached_prompt_hit_ratio",
		"agent_user_memory_total", "agent_user_memory_injected", "agent_user_memory_omitted",
		"agent_user_memory_section_runes", "agent_user_memory_truncated",
		"agent_model_tool_catalog_count", "agent_model_tool_catalog_schema_runes",
		"agent_model_tool_bound_count", "agent_model_tool_bound_schema_runes",
	} {
		if !strings.Contains(body, name) {
			t.Fatalf("/debug/metrics 缺少度量 %q", name)
		}
	}
	if !json.Valid(recorder.Body.Bytes()) {
		t.Fatal("/debug/metrics 输出非法 JSON")
	}
	// P1 回归护栏：无鉴权的 /debug/metrics 绝不能暴露 expvar 默认的 cmdline（os.Args 可能含 -token）。
	if strings.Contains(body, "cmdline") {
		t.Fatal("/debug/metrics 不应包含 cmdline（可能泄漏命令行密钥）")
	}

	// 验收 1：日志文件含回合开始/结束结构化记录（按 draft 归属过滤）。
	_ = closer.Close()
	file, err := os.Open(filepath.Join(logDir, "agenttest.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	var sawStart, sawEnd, sawModelCall bool
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			continue
		}
		if record["draft_id"] != "draft_metrics" {
			continue
		}
		switch record["msg"] {
		case "turn_started":
			sawStart = true
		case "turn_ended":
			sawEnd = true
			if record["outcome"] != "finished" || record["turn_id"] == nil || record["duration_ms"] == nil {
				t.Fatalf("turn_ended 结构化字段缺失: %+v", record)
			}
		case "model_call":
			// AC1：每次模型调用延迟的结构化记录。
			if record["latency_ms"] == nil {
				t.Fatalf("model_call 记录应含 latency_ms: %+v", record)
			}
			sawModelCall = true
		}
	}
	if !sawStart || !sawEnd || !sawModelCall {
		t.Fatalf("日志应含 turn_started(%v)/turn_ended(%v)/model_call(%v) 记录", sawStart, sawEnd, sawModelCall)
	}

	// P1 回归护栏：结构化日志绝不含用户话原文与其中的伪密钥（结构化日志只记 ID/计数，不记内容）。
	rawLog, err := os.ReadFile(filepath.Join(logDir, "agenttest.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"SENTINEL_SECRET_do_not_persist_9z8y7x", userUtterance} {
		if strings.Contains(string(rawLog), forbidden) {
			t.Fatalf("日志泄漏了用户内容/密钥: %q", forbidden)
		}
	}
}
