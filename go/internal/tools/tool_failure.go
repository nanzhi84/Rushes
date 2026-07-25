package tools

// DefaultToolFailureRecovery 是缺省恢复指引。
const DefaultToolFailureRecovery = "读取 observation 与 data 定位失败原因后修正参数重试；不要原样重发失败请求。"

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
