package telemetry

import (
	"expvar"
	"net/http"
)

// Handler 返回 expvar 的 JSON 处理器，把所有已注册度量（本包 Counter/Gauge/Histogram/比值
// 及运行时 memstats）暴露出去。挂在 /debug/metrics，不入 OpenAPI 冻结面（#95 H3）。
func Handler() http.Handler {
	return expvar.Handler()
}
