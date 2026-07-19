package telemetry

import (
	"expvar"
	"fmt"
	"net/http"
	"strings"
)

// Handler 返回一个 JSON 度量端点，暴露本包 Counter/Gauge/Histogram/比值与运行时 memstats，
// 但**跳过 expvar 默认发布的 cmdline**（os.Args）——进程命令行可能带 -token 等密钥，而
// /debug/metrics 无鉴权（#95 H3 P1）。输出格式与 expvar.Handler 一致，只是漏掉 cmdline。
// 挂在 /debug 下，不入 OpenAPI 冻结面。
func Handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		var builder strings.Builder
		builder.WriteString("{\n")
		first := true
		expvar.Do(func(entry expvar.KeyValue) {
			if entry.Key == "cmdline" {
				return
			}
			if !first {
				builder.WriteString(",\n")
			}
			first = false
			_, _ = fmt.Fprintf(&builder, "%q: %s", entry.Key, entry.Value)
		})
		builder.WriteString("\n}\n")
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = writer.Write([]byte(builder.String()))
	})
}
