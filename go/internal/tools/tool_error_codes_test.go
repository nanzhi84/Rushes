package tools

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// internalRoot 定位 go/internal 目录，供守卫测试扫描兄弟包源码。worker/media 因 depguard
// 禁止 import tools，其 error_code 只能以字面量出现，只能靠源码扫描而非编译期引用来校验其
// 属于中央注册集合（#95 T2）。
func internalRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位当前测试文件路径")
	}
	// file 形如 .../go/internal/tools/tool_error_codes_test.go
	return filepath.Dir(filepath.Dir(file))
}

// errorCodeLiteralPatterns 覆盖仓库里 error_code 字面量的三种写法：JSON 键映射、结构体
// 字面量字段、结构体字段赋值。结构体字段定义（ErrorCode string `json:"error_code,..."`）
// 与字段读取（item.ErrorCode = issue.ErrorCode）都不带 `: "值"`，刻意不匹配。
//
// 已知边界（评审记录）：
//   - 扫描器不解析 Go 语法，注释里出现的 error_code 字面量也会被计入。这是刻意从严——宁可
//     要求注释示例码也登记，也不放过真实漏登记；文档若要举一个未登记的假想码，避开
//     `"error_code": "x"` 这个精确形态即可。
//   - 间接置码路径（如 plan.update 经 planUpdateFailure 把 data["reason"] 复制进 error_code）
//     不产生 `"error_code": "literal"` 形态，扫描器看不到。这类路径靠「reason 值一律引用
//     ErrCode* 常量」的约定保证登记，而非本扫描；agent/agentexec 零裸字面量的守卫兜底了
//     「有人改回裸串」的回归。
var errorCodeLiteralPatterns = []*regexp.Regexp{
	regexp.MustCompile(`"error_code"\s*:\s*"([^"]+)"`),
	regexp.MustCompile(`\bErrorCode\s*:\s*"([^"]+)"`),
	regexp.MustCompile(`\.ErrorCode\s*=\s*"([^"]+)"`),
}

type errorCodeLiteral struct {
	file string
	code string
}

func scanErrorCodeLiterals(t *testing.T, dir string) []errorCodeLiteral {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录 %s 失败: %v", dir, err)
	}
	var found []errorCodeLiteral
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取文件 %s 失败: %v", path, err)
		}
		for _, pattern := range errorCodeLiteralPatterns {
			for _, match := range pattern.FindAllStringSubmatch(string(data), -1) {
				found = append(found, errorCodeLiteral{file: path, code: match[1]})
			}
		}
	}
	return found
}

// TestEveryErrorCodeLiteralIsRegistered 是 error_code 事实源棘轮（类比 schema 棘轮）：
// 工具失败信封四个来源包里出现的每一个 error_code 字面量都必须属于中央注册集合。新增未登记
// 的 error_code（无论在能否 import tools 的包里）都会让本测试变红，倒逼先在
// tool_error_codes.go 登记。
func TestEveryErrorCodeLiteralIsRegistered(t *testing.T) {
	root := internalRoot(t)
	for _, pkg := range []string{"agent", "agentexec", "worker", "media"} {
		for _, literal := range scanErrorCodeLiterals(t, filepath.Join(root, pkg)) {
			if !ToolErrorCodeRegistered(literal.code) {
				t.Errorf("%s 出现未注册的 error_code 字面量 %q；请先在 tools/tool_error_codes.go 登记再使用",
					literal.file, literal.code)
			}
		}
	}
}

// TestConstantMigratedPackagesHaveNoRawErrorCodeLiterals 守护「能 import tools 的包一律
// 改引常量」这一收口：agent/agentexec 扫描后不得残留任何裸 error_code 字面量。
func TestConstantMigratedPackagesHaveNoRawErrorCodeLiterals(t *testing.T) {
	root := internalRoot(t)
	for _, pkg := range []string{"agent", "agentexec"} {
		for _, literal := range scanErrorCodeLiterals(t, filepath.Join(root, pkg)) {
			t.Errorf("%s 仍有裸 error_code 字面量 %q；应改引 tools.ErrCodeXxx 常量", literal.file, literal.code)
		}
	}
}

// TestWorkerAndMediaLiteralsRemainCovered 明示 worker/media 因 depguard 保留字面量的边界：
// 它们至少各自保留已知的失败码，且都在注册集合内（其余通用性由上面的棘轮覆盖）。
func TestWorkerAndMediaLiteralsRemainCovered(t *testing.T) {
	root := internalRoot(t)
	present := map[string]bool{}
	for _, pkg := range []string{"worker", "media"} {
		for _, literal := range scanErrorCodeLiterals(t, filepath.Join(root, pkg)) {
			present[literal.code] = true
		}
	}
	for _, code := range []ToolErrorCode{
		ErrCodeStaleRecoveryExhausted, ErrCodeJobHandlerFailed, ErrCodePreviewDecodeFailed,
	} {
		if !present[string(code)] {
			t.Errorf("预期 worker/media 源码仍含字面量 %q，未扫描到；若已迁移请同步本用例与注册集合", code)
		}
	}
}

// TestRegisteredToolErrorCodesAreWellFormed 保证注册集合本身无重复、无空值。
func TestRegisteredToolErrorCodesAreWellFormed(t *testing.T) {
	seen := map[ToolErrorCode]struct{}{}
	for _, code := range allToolErrorCodes {
		if strings.TrimSpace(string(code)) == "" {
			t.Error("error_code 注册集合出现空取值")
		}
		if _, dup := seen[code]; dup {
			t.Errorf("error_code 注册集合出现重复取值 %q", code)
		}
		seen[code] = struct{}{}
	}
}

// TestRegisteredToolStatusesAreWellFormed 对 ToolStatus 集合做同样的完整性校验。
func TestRegisteredToolStatusesAreWellFormed(t *testing.T) {
	seen := map[ToolStatus]struct{}{}
	for _, status := range allToolStatuses {
		if strings.TrimSpace(string(status)) == "" {
			t.Error("ToolStatus 注册集合出现空取值")
		}
		if _, dup := seen[status]; dup {
			t.Errorf("ToolStatus 注册集合出现重复取值 %q", status)
		}
		seen[status] = struct{}{}
	}
}

// TestToolVocabularyAccessors 覆盖对外查询接口的正反两路，兼作事实源公共 API 的回归。
func TestToolVocabularyAccessors(t *testing.T) {
	if !ToolErrorCodeRegistered(string(ErrCodeUnknownTool)) {
		t.Error("已声明的 error_code 应被识别为已注册")
	}
	if ToolErrorCodeRegistered("definitely_not_registered") {
		t.Error("未声明的 error_code 不应被识别为已注册")
	}
	if got := listRegisteredToolErrorCodes(); len(got) != len(allToolErrorCodes) {
		t.Errorf("listRegisteredToolErrorCodes 返回 %d 项，期望 %d", len(got), len(allToolErrorCodes))
	}
	if !toolStatusRegistered(string(StatusSucceeded)) {
		t.Error("已声明的 status 应被识别为已注册")
	}
	if toolStatusRegistered("definitely_not_a_status") {
		t.Error("未声明的 status 不应被识别为已注册")
	}
	if got := listRegisteredToolStatuses(); len(got) != len(allToolStatuses) {
		t.Errorf("listRegisteredToolStatuses 返回 %d 项，期望 %d", len(got), len(allToolStatuses))
	}
}
