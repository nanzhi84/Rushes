package agentexec

import (
	"context"

	"github.com/cloudwego/eino/components/model"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

// newTestExecutor 构造一个仅注入 DB（analyzer 默认、无 speechRecognizer/progress）的
// 领域执行器，签名与 agent.NewService 对齐，便于执行器测试从引擎侧原样搬迁。
func newTestExecutor(_ context.Context, database *storage.DB, _ model.ToolCallingChatModel) (*Executor, error) {
	return New(database, understanding.NewAnalyzer(nil), nil, nil), nil
}
