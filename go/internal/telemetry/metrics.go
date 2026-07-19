// Package telemetry 是纯 $gostd 叶子包，提供进程级度量（经 expvar 暴露到 /debug/metrics）
// 与结构化 JSON 日志落盘（按大小轮转）。领域包在此之上定义自己的具名度量，引擎/入口负责
// 把 Handler 挂到 HTTP 路由、把日志器装到 cmd 入口。它不认识任何业务类型，也不入 OpenAPI
// 冻结面（#95 H3）。
package telemetry

import (
	"expvar"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// getOrPublishInt 幂等注册一个 expvar.Int：已存在同名则复用，避免重复注册 panic（多次
// 构造同名度量、测试重跑都安全）。
func getOrPublishInt(name string) *expvar.Int {
	if existing := expvar.Get(name); existing != nil {
		if value, ok := existing.(*expvar.Int); ok {
			return value
		}
		panic("telemetry: 度量名 " + name + " 已被非 Int 类型占用")
	}
	value := new(expvar.Int)
	expvar.Publish(name, value)
	return value
}

// Counter 是单调递增计数器。
type Counter struct{ value *expvar.Int }

// NewCounter 注册并返回一个具名计数器。
func NewCounter(name string) *Counter { return &Counter{value: getOrPublishInt(name)} }

// Inc 加一。
func (c *Counter) Inc() { c.value.Add(1) }

// Add 累加 delta（应为非负）。
func (c *Counter) Add(delta int64) { c.value.Add(delta) }

// Value 返回当前值。
func (c *Counter) Value() int64 { return c.value.Value() }

// Gauge 是可增可减、可赋值的瞬时量（如队列深度）。
type Gauge struct{ value *expvar.Int }

// NewGauge 注册并返回一个具名量表。
func NewGauge(name string) *Gauge { return &Gauge{value: getOrPublishInt(name)} }

// Add 增减 delta。
func (g *Gauge) Add(delta int64) { g.value.Add(delta) }

// Set 赋值。
func (g *Gauge) Set(value int64) { g.value.Set(value) }

// Value 返回当前值。
func (g *Gauge) Value() int64 { return g.value.Value() }

// Histogram 记录一列观测值的分布：计数、总和、最小、最大与固定上界桶。用于延迟（毫秒）与
// token 规模等分布型度量。实现 expvar.Var，String 返回稳定 JSON。
type Histogram struct {
	mu      sync.Mutex
	count   int64
	sum     int64
	min     int64
	max     int64
	bounds  []int64
	buckets []int64
}

// NewHistogram 注册并返回一个具名直方图。bounds 是升序的桶上界（含），观测值落入第一个
// >= 它的桶；超过所有上界的落入溢出桶。bounds 为空时退化为「只统计 count/sum/min/max」。
func NewHistogram(name string, bounds []int64) *Histogram {
	sorted := append([]int64(nil), bounds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	histogram := &Histogram{bounds: sorted, buckets: make([]int64, len(sorted)+1)}
	if expvar.Get(name) == nil {
		expvar.Publish(name, histogram)
	}
	return histogram
}

// Observe 记录一个观测值。
func (h *Histogram) Observe(value int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.count == 0 || value < h.min {
		h.min = value
	}
	if h.count == 0 || value > h.max {
		h.max = value
	}
	h.count++
	h.sum += value
	index := sort.Search(len(h.bounds), func(i int) bool { return value <= h.bounds[i] })
	h.buckets[index]++
}

// Snapshot 返回当前分布的只读拷贝，供测试与派生度量读取。
func (h *Histogram) Snapshot() (count, sum, min, max int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count, h.sum, h.min, h.max
}

// String 实现 expvar.Var，输出稳定字段顺序的 JSON。
func (h *Histogram) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var builder strings.Builder
	average := 0.0
	if h.count > 0 {
		average = float64(h.sum) / float64(h.count)
	}
	fmt.Fprintf(&builder, `{"count":%d,"sum":%d,"min":%d,"max":%d,"avg":%.2f,"buckets":{`,
		h.count, h.sum, h.min, h.max, average)
	for index, bound := range h.bounds {
		if index > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, `"%d":%d`, bound, h.buckets[index])
	}
	if len(h.bounds) > 0 {
		builder.WriteByte(',')
	}
	fmt.Fprintf(&builder, `"+Inf":%d}}`, h.buckets[len(h.buckets)-1])
	return builder.String()
}

// PublishRatio 暴露一个派生比值度量（如缓存命中率），值由 compute 每次读取时计算。
func PublishRatio(name string, compute func() float64) {
	if expvar.Get(name) != nil {
		return
	}
	expvar.Publish(name, expvar.Func(func() any {
		value := compute()
		return jsonFloat(value)
	}))
}

// jsonFloat 保证 NaN/Inf 序列化为 0，避免 expvar 输出非法 JSON。
func jsonFloat(value float64) float64 {
	if value != value || value > 1e308 || value < -1e308 {
		return 0
	}
	return value
}
