package tools

import "testing"

// TestEnsureFailureRecovery 直接覆盖失败闭包收口用的兜底逻辑两个分支：缺失时回退
// DefaultToolFailureRecovery、已有时原样保留，且不丢失其它键（#95 T5 评审）。
func TestEnsureFailureRecovery(t *testing.T) {
	t.Parallel()

	// nil data → 返回新 map 且带兜底 recovery。
	if got, _ := EnsureFailureRecovery(nil)["recovery"].(string); got != DefaultToolFailureRecovery {
		t.Errorf("nil data 应回退兜底 recovery，实际 %q", got)
	}

	// 空 recovery → 兜底，且既有键保留。
	filled := EnsureFailureRecovery(map[string]any{"reason": "x"})
	if got, _ := filled["recovery"].(string); got != DefaultToolFailureRecovery {
		t.Errorf("空 recovery 应回退兜底，实际 %q", got)
	}
	if filled["reason"] != "x" {
		t.Error("EnsureFailureRecovery 丢失了既有键 reason")
	}

	// 已有具体 recovery → 保持不变。
	kept := EnsureFailureRecovery(map[string]any{"recovery": "具体指引"})
	if got, _ := kept["recovery"].(string); got != "具体指引" {
		t.Errorf("已有 recovery 应保持不变，实际 %q", got)
	}
}
