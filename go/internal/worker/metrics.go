package worker

import (
	"time"

	"github.com/nanzhi84/Rushes/go/internal/telemetry"
)

// job 生命周期度量（#95 H3）：claim→terminal 延迟分布与终态分类。
var (
	metricJobClaimToTerminalMS = telemetry.NewHistogram(
		"worker_job_claim_to_terminal_ms",
		[]int64{100, 500, 1000, 5000, 30000, 120000, 600000},
	)
	metricJobSucceeded = telemetry.NewCounter("worker_job_succeeded_total")
	metricJobFailed    = telemetry.NewCounter("worker_job_failed_total")
)

// recordJobTerminalMetrics 在 job 写入终态后记录终态分类与 claim→terminal 延迟。started_at
// 缺失或不可解析时只记分类、不记延迟（尽力而为，绝不阻断终态）。
func recordJobTerminalMetrics(eventType string, job Job, now time.Time) {
	switch eventType {
	case "JobSucceeded":
		metricJobSucceeded.Inc()
	case "JobFailed":
		metricJobFailed.Inc()
	}
	startedAt, err := time.Parse(time.RFC3339Nano, value(job.StartedAt))
	if err != nil {
		return
	}
	if latency := now.Sub(startedAt).Milliseconds(); latency >= 0 {
		metricJobClaimToTerminalMS.Observe(latency)
	}
}
