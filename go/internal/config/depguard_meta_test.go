package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDepguardRulesCoverEveryInternalPackage 防止 depguard 因 glob 或新包漏登记而静默 no-op。
// integration 当前只有 _test.go；仓库规则明确 _test.go 不纳入分层守护，因此显式豁免。
func TestDepguardRulesCoverEveryInternalPackage(t *testing.T) {
	configPath := filepath.Join("..", "..", ".golangci.yml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config := string(data)
	internalRoot := filepath.Join("..")
	entries, err := os.ReadDir(internalRoot)
	if err != nil {
		t.Fatal(err)
	}
	exempt := map[string]string{
		"integration": "纯跨包集成测试目录；_test.go 已按仓库规则统一排除 depguard",
	}
	packages := 0
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		goFiles, err := filepath.Glob(filepath.Join(internalRoot, entry.Name(), "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		if len(goFiles) == 0 {
			continue
		}
		packages++
		if reason := exempt[entry.Name()]; reason != "" {
			t.Logf("depguard 显式豁免 internal/%s: %s", entry.Name(), reason)
			continue
		}
		direct := `"**/internal/` + entry.Name() + `/*.go"`
		nested := `"**/internal/` + entry.Name() + `/**/*.go"`
		if !strings.Contains(config, direct) || !strings.Contains(config, nested) {
			t.Errorf(
				"internal/%s 缺少 depguard 直属+子目录双 glob: direct=%v nested=%v",
				entry.Name(), strings.Contains(config, direct), strings.Contains(config, nested),
			)
		}
	}
	if packages < 16 {
		t.Fatalf("internal 包盘点异常: got=%d want>=16", packages)
	}
}
