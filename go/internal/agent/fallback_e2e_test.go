//go:build e2e_scaffold

package agent

import (
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
)

func TestE2EFallbackScaffoldDeclinesOrdinaryInput(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if service.fallbackScaffold == nil {
		t.Fatal("e2e_scaffold 构建必须安装 fallback scaffold")
	}
	reply, handled, err := service.fallbackScaffold.TryHandle(
		t.Context(), "missing", "message", "普通产品输入",
	)
	if err != nil || handled || reply != "" {
		t.Fatalf("reply=%q handled=%v err=%v", reply, handled, err)
	}
}
