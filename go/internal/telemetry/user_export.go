package telemetry

var (
	metricUserExportRequested = NewCounter("user_export_requested_total")
	metricUserExportSucceeded = NewCounter("user_export_succeeded_total")
	metricUserExportFailed    = NewCounter("user_export_failed_total")
)

// RecordUserExportRequested 只记录已被领域服务接受的用户创建/重试请求。
func RecordUserExportRequested() { metricUserExportRequested.Inc() }

// RecordUserExportTerminal 只接受固定终态，不附带 draft/job 等高基数字段。
func RecordUserExportTerminal(succeeded bool) {
	if succeeded {
		metricUserExportSucceeded.Inc()
		return
	}
	metricUserExportFailed.Inc()
}
