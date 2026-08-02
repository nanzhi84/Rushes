package tools

// 本文件是工具结果信封词表的唯一事实源（#95 T2）：ToolStatus 覆盖 ToolResult.Status 的
// 取值域，ToolErrorCode 覆盖 ToolResult.Data["error_code"] 的失败分类。此前这些字面量散落
// 在 agent/agentexec/worker/media 十余个文件，"stale_recovery_exhausted" 等还两处手写；
// 收敛到这里后，能 import tools 的包（agent/agentexec）一律改引常量，depguard 禁止 import
// tools 的包（worker/media）保留字面量但由 tool_error_codes_test.go 的扫描守卫断言其属于
// 注册集合。常量值与迁移前的字面量逐字相同，本改动零行为变化。

// ToolStatus 是工具结果信封 ToolResult.Status 的规范取值。
type ToolStatus string

const (
	StatusSucceeded        ToolStatus = "succeeded"
	StatusFailed           ToolStatus = "failed"
	StatusValidationFailed ToolStatus = "validation_failed"
	StatusCancelled        ToolStatus = "cancelled"
	StatusTimeout          ToolStatus = "timeout"
	StatusWaiting          ToolStatus = "waiting_user"
)

// ToolErrorCode 是工具失败分类的中央枚举与唯一事实源。
type ToolErrorCode string

const (
	// —— 编排与恢复（agent/tool_recovery.go、interceptor.go、tool_execution.go）——
	ErrCodeUnknownTool          ToolErrorCode = "unknown_tool"
	ErrCodeFailureSerialization ToolErrorCode = "failure_serialization_error"
	ErrCodeToolExecutionError   ToolErrorCode = "tool_execution_error"
	ErrCodeToolValidationFailed ToolErrorCode = "tool_validation_failed"
	ErrCodeToolCancelled        ToolErrorCode = "tool_cancelled"
	ErrCodeToolTimeout          ToolErrorCode = "tool_timeout"
	// ErrCodeToolNotInSurface 由 #141 动态工具面拦截器产出。
	ErrCodeToolNotInSurface ToolErrorCode = "tool_not_in_surface"
	// ErrCodeConfirmationRequired 由 #128 的破坏性强制确认拦截器产出。
	ErrCodeConfirmationRequired      ToolErrorCode = "confirmation_required"
	ErrCodeInvalidConfirmationTarget ToolErrorCode = "invalid_confirmation_target"

	// —— 时间线补丁（agentexec/shared_util.go、timeline_op_recovery.go、tool_exec_timeline.go）——
	ErrCodeTimelineOpSemanticError  ToolErrorCode = "timeline_op_semantic_error"
	ErrCodeTimelineOpFieldError     ToolErrorCode = "timeline_op_field_error"
	ErrCodeTimelineRangeOutOfBounds ToolErrorCode = "timeline_range_out_of_bounds"
	ErrCodeTimelineAbsent           ToolErrorCode = "timeline_absent"
	ErrCodeStaleTarget              ToolErrorCode = "stale_target"

	// —— 内容合同校验（agentexec/content_contract.go）——
	ErrCodeMissingBeatGrid ToolErrorCode = "missing_beat_grid"

	// —— 创作计划本（agentexec/plan_update.go，经 planUpdateFailure 把 reason 复制到 error_code）——
	ErrCodePlanRequired      ToolErrorCode = "plan_required"
	ErrCodePlanNotJSON       ToolErrorCode = "plan_not_json"
	ErrCodeContractInvalid   ToolErrorCode = "contract_invalid"
	ErrCodeContractNotJSON   ToolErrorCode = "contract_not_json"
	ErrCodeReservedKey       ToolErrorCode = "reserved_key"
	ErrCodeStoredReservedKey ToolErrorCode = "stored_reserved_key"
	ErrCodePlanTooLarge      ToolErrorCode = "plan_too_large"
	ErrCodePlanConflict      ToolErrorCode = "plan_conflict"

	// —— 长期记忆（agentexec/tool_exec_memory.go）——
	ErrCodeMemoryEvidenceUnavailable  ToolErrorCode = "memory_evidence_unavailable"
	ErrCodeMemoryMutationEmpty        ToolErrorCode = "memory_mutation_empty"
	ErrCodeMemoryEntriesLimit         ToolErrorCode = "memory_entries_limit"
	ErrCodeMemoryRemoveLimit          ToolErrorCode = "memory_remove_limit"
	ErrCodeMemoryKeyInvalid           ToolErrorCode = "memory_key_invalid"
	ErrCodeMemoryKeyDuplicate         ToolErrorCode = "memory_key_duplicate"
	ErrCodeMemoryKindInvalid          ToolErrorCode = "memory_kind_invalid"
	ErrCodeMemoryStatementInvalid     ToolErrorCode = "memory_statement_invalid"
	ErrCodeMemoryEvidenceQuoteInvalid ToolErrorCode = "memory_evidence_quote_invalid"
	ErrCodeMemoryRemoveKeyInvalid     ToolErrorCode = "memory_remove_key_invalid"
	ErrCodeMemoryRemoveKeyDuplicate   ToolErrorCode = "memory_remove_key_duplicate"
	ErrCodeMemoryEvidenceInvalid      ToolErrorCode = "memory_evidence_invalid"
	ErrCodeMemoryInputInvalid         ToolErrorCode = "memory_input_invalid"

	// —— 素材理解（agentexec/tool_exec_understand.go）——
	ErrCodeUnderstandingFailed ToolErrorCode = "understanding_failed"
	// —— 素材检索与渐进式深搜（agentexec/speech_inspect.go、shot_search.go、shot_deep_search.go）——
	ErrCodeIndexMissing           ToolErrorCode = "index_missing"
	ErrCodeShotIndexNotReady      ToolErrorCode = "shot_index_not_ready"
	ErrCodeShotIndexFailed        ToolErrorCode = "shot_index_failed"
	ErrCodeShotIndexSnapshotStale ToolErrorCode = "shot_index_snapshot_stale"
	ErrCodeShotRefNotFound        ToolErrorCode = "shot_ref_not_found"
	ErrCodeShotRefAssetMismatch   ToolErrorCode = "shot_ref_asset_mismatch"
	ErrCodeShotDeepInputInvalid   ToolErrorCode = "shot_deep_input_invalid"
	ErrCodeShotDeepVisionMissing  ToolErrorCode = "shot_deep_vision_unavailable"
	ErrCodeShotDeepAnalysisFailed ToolErrorCode = "shot_deep_analysis_failed"

	// —— 预览质检（media/render.go 因 depguard 保留字面量）——
	ErrCodePreviewDecodeFailed ToolErrorCode = "preview_decode_failed"

	// —— 后台 job（worker/job.go、worker/runner.go；worker 因 depguard 保留字面量）——
	ErrCodeStaleRecoveryExhausted ToolErrorCode = "stale_recovery_exhausted"
	ErrCodeJobHandlerFailed       ToolErrorCode = "job_handler_failed"
)

// allToolErrorCodes 是注册集合的单一枚举列表，顺序与上面的常量块一致。守卫测试据此校验
// 「源码中出现的 error_code 字面量都属于本集合」，也校验无重复取值。
var allToolErrorCodes = []ToolErrorCode{
	ErrCodeUnknownTool,
	ErrCodeFailureSerialization,
	ErrCodeToolExecutionError,
	ErrCodeToolValidationFailed,
	ErrCodeToolCancelled,
	ErrCodeToolTimeout,
	ErrCodeToolNotInSurface,
	ErrCodeConfirmationRequired,
	ErrCodeInvalidConfirmationTarget,
	ErrCodeTimelineOpSemanticError,
	ErrCodeTimelineOpFieldError,
	ErrCodeTimelineRangeOutOfBounds,
	ErrCodeTimelineAbsent,
	ErrCodeStaleTarget,
	ErrCodeMissingBeatGrid,
	ErrCodePlanRequired,
	ErrCodePlanNotJSON,
	ErrCodeContractInvalid,
	ErrCodeContractNotJSON,
	ErrCodeReservedKey,
	ErrCodeStoredReservedKey,
	ErrCodePlanTooLarge,
	ErrCodePlanConflict,
	ErrCodeMemoryEvidenceUnavailable,
	ErrCodeMemoryMutationEmpty,
	ErrCodeMemoryEntriesLimit,
	ErrCodeMemoryRemoveLimit,
	ErrCodeMemoryKeyInvalid,
	ErrCodeMemoryKeyDuplicate,
	ErrCodeMemoryKindInvalid,
	ErrCodeMemoryStatementInvalid,
	ErrCodeMemoryEvidenceQuoteInvalid,
	ErrCodeMemoryRemoveKeyInvalid,
	ErrCodeMemoryRemoveKeyDuplicate,
	ErrCodeMemoryEvidenceInvalid,
	ErrCodeMemoryInputInvalid,
	ErrCodeUnderstandingFailed,
	ErrCodeIndexMissing,
	ErrCodeShotIndexNotReady,
	ErrCodeShotIndexFailed,
	ErrCodeShotIndexSnapshotStale,
	ErrCodeShotRefNotFound,
	ErrCodeShotRefAssetMismatch,
	ErrCodeShotDeepInputInvalid,
	ErrCodeShotDeepVisionMissing,
	ErrCodeShotDeepAnalysisFailed,
	ErrCodePreviewDecodeFailed,
	ErrCodeStaleRecoveryExhausted,
	ErrCodeJobHandlerFailed,
}

var registeredToolErrorCodes = func() map[ToolErrorCode]struct{} {
	set := make(map[ToolErrorCode]struct{}, len(allToolErrorCodes))
	for _, code := range allToolErrorCodes {
		set[code] = struct{}{}
	}
	return set
}()

var allToolStatuses = []ToolStatus{
	StatusSucceeded, StatusFailed, StatusValidationFailed, StatusCancelled, StatusTimeout, StatusWaiting,
}

var registeredToolStatuses = func() map[ToolStatus]struct{} {
	set := make(map[ToolStatus]struct{}, len(allToolStatuses))
	for _, status := range allToolStatuses {
		set[status] = struct{}{}
	}
	return set
}()

// ToolErrorCodeRegistered 报告给定字面量是否属于中央注册集合。它支撑 error_code 扫描守卫，
// 是本文件唯一对外导出的查询接口；其余查询辅助仅测试消费，保持非导出。
func ToolErrorCodeRegistered(code string) bool {
	_, ok := registeredToolErrorCodes[ToolErrorCode(code)]
	return ok
}

// listRegisteredToolErrorCodes 返回注册集合的副本，仅供同包测试做完整性/诊断使用。
func listRegisteredToolErrorCodes() []ToolErrorCode {
	return append([]ToolErrorCode(nil), allToolErrorCodes...)
}

// toolStatusRegistered 报告给定字面量是否属于规范状态集合（不含 render 透传的 job 状态），
// 仅供同包测试使用。
func toolStatusRegistered(status string) bool {
	_, ok := registeredToolStatuses[ToolStatus(status)]
	return ok
}

// listRegisteredToolStatuses 返回规范状态集合的副本，仅供同包测试使用。
func listRegisteredToolStatuses() []ToolStatus {
	return append([]ToolStatus(nil), allToolStatuses...)
}
