package tools

// DefaultToolFailureRecovery 是缺省恢复指引，供没有专门 recovery 文案的结构化失败兜底
// （#95 T5）。让「每个结构化失败必带非空 recovery」成为机械保证而非抽样成立：ToolFailure、
// 以及无法走 ToolFailure（不带 error_code）的领域失败闭包都以它收口空 recovery。
const DefaultToolFailureRecovery = "读取 observation 与 data 定位失败原因后修正参数重试；不要原样重发失败请求。"

// EnsureFailureRecovery 保证失败 Data 带非空 recovery：缺失时写入 DefaultToolFailureRecovery。
// 供无法走 ToolFailure（不带 error_code）的领域失败闭包（talking_head / recut / beat_mix 的
// failed 兜底）收口空 recovery，使「每个结构化失败必带 recovery」成为机械保证而非抽样。传入
// nil 时返回一个新 map；否则原地补齐并返回同一个 map，便于链式使用。
func EnsureFailureRecovery(data map[string]any) map[string]any {
	if data == nil {
		data = map[string]any{}
	}
	if recovery, _ := data["recovery"].(string); recovery == "" {
		data["recovery"] = DefaultToolFailureRecovery
	}
	return data
}

// ToolFailure 是结构化工具失败的共享构造器（#95 T5）：统一 status / observation / error_code /
// recovery 信封，并保证每个失败都带非空、可执行的 recovery 指引。error_code 与 recovery 由本
// 构造器写入 Data，其余上下文经 extra 并入（调用方不要在 extra 里重复这两个键）。recovery 为
// 空时回退到通用兜底文案，从而让「每个结构化失败必带非空 recovery」成为构造期不变量，而不是
// 依赖各调用点自觉。
func ToolFailure(
	status ToolStatus,
	observation string,
	errorCode ToolErrorCode,
	recovery string,
	extra map[string]any,
) ToolResult {
	if recovery == "" {
		recovery = DefaultToolFailureRecovery
	}
	data := make(map[string]any, len(extra)+2)
	for key, value := range extra {
		data[key] = value
	}
	data["error_code"] = string(errorCode)
	data["recovery"] = recovery
	return ToolResult{Status: string(status), Observation: observation, Data: data}
}
