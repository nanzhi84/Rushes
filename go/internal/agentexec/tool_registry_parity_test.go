package agentexec

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// TestExecutorRoutesEveryRegisteredLLMTool 是 registry↔executor 的 parity 棘轮（#95 T3）。
// registry 的 builders 与 Executor.ExecuteTool 的分发 switch 曾是双事实源，漏写 case 只有
// 运行时才报「工具未注册执行器」。这里遍历 Specs() 全部 ExposureLLM 工具，以最小合法参数
// （对应输入类型的零值）调 ExecuteTool，断言不落到 default 未注册分支——新增工具漏写 case
// 即变红，无需等真实调用暴露。
func TestExecutorRoutesEveryRegisteredLLMTool(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := rushestools.NewRegistry(database, exec)
	if err != nil {
		t.Fatal(err)
	}
	const draftID = "draft_registry_parity"
	agenttest.CreateAgentDraft(t, database, draftID)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	routed := 0
	for _, spec := range registry.Specs(true) {
		if spec.Exposure != rushestools.ExposureLLM {
			continue
		}
		if spec.InputType == nil {
			t.Errorf("%s 缺少 InputType，无法构造最小参数", spec.Name)
			continue
		}
		input := reflect.New(spec.InputType).Elem().Interface()
		if unrouted := executorFailedToRoute(ctx, exec, spec.Name, input); unrouted != "" {
			t.Errorf("%s 未在 Executor.ExecuteTool 声明 case：%s", spec.Name, unrouted)
		}
		routed++
	}
	if routed == 0 {
		t.Fatal("没有遍历到任何 ExposureLLM 工具，parity 断言落空")
	}
}

// executorFailedToRoute 只判定 ExecuteTool 是否落到 default「工具未注册执行器」分支。领域层
// 因空参数或空 draft 返回的业务错误、甚至 panic，都说明 case 存在、路由成立，返回空串。
func executorFailedToRoute(ctx context.Context, exec *Executor, name string, input any) (unrouted string) {
	defer func() {
		// case 命中后领域逻辑对零值参数的 panic 与 parity 无关，恢复即视为已路由。
		_ = recover()
	}()
	if _, err := exec.ExecuteTool(ctx, name, input); err != nil &&
		strings.Contains(err.Error(), "工具未注册执行器") {
		return err.Error()
	}
	return ""
}

// TestExecutorRejectsUnroutedTool 证明上面的 parity 检测非空断言：未在 switch 声明的工具名
// 必然落到 default 分支返回「工具未注册执行器」，因此上面的遍历若漏 case 一定能捕获。
func TestExecutorRejectsUnroutedTool(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	const draftID = "draft_registry_parity_negative"
	agenttest.CreateAgentDraft(t, database, draftID)
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	if _, execErr := exec.ExecuteTool(ctx, "fake.unrouted_tool", struct{}{}); execErr == nil ||
		!strings.Contains(execErr.Error(), "工具未注册执行器") {
		t.Fatalf("未注册工具应返回「工具未注册执行器」，实际 err=%v", execErr)
	}
}
