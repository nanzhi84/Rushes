package agent

import "github.com/nanzhi84/Rushes/go/internal/contracts"

// IsAgentWaitedJobKind 暂为旧取消/重发兼容保留；它只分类既有 job，绝不创建、
// 派发或续跑 synthetic Agent turn。旧 observation 表也不再被运行时消费。
func IsAgentWaitedJobKind(kind string) bool {
	spec, exists := contracts.LookupJobKind(kind)
	return exists && spec.AgentWaited
}
