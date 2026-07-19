package reducer

import "github.com/nanzhi84/Rushes/go/internal/telemetry"

// metricWriteTxMS 记录唯一业务写路径（reducer.Apply 的 immediate transaction）从开启到成功
// 提交的耗时分布，单位毫秒；回滚的事务不计入（#95 H3）。
var metricWriteTxMS = telemetry.NewHistogram(
	"reducer_write_tx_ms", []int64{1, 5, 10, 25, 50, 100, 250, 1000},
)
