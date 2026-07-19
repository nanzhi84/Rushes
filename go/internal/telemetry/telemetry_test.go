package telemetry

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCounterAndGauge(t *testing.T) {
	counter := NewCounter("test_counter_basic")
	counter.Inc()
	counter.Add(4)
	if counter.Value() != 5 {
		t.Fatalf("counter=%d want 5", counter.Value())
	}
	// 幂等注册：同名再取复用同一底层。
	if NewCounter("test_counter_basic").Value() != 5 {
		t.Fatal("同名 counter 应复用状态")
	}
	gauge := NewGauge("test_gauge_basic")
	gauge.Set(10)
	gauge.Add(-3)
	if gauge.Value() != 7 {
		t.Fatalf("gauge=%d want 7", gauge.Value())
	}
}

func TestHistogramObserveAndString(t *testing.T) {
	histogram := NewHistogram("test_hist_basic", []int64{10, 100})
	for _, value := range []int64{5, 50, 50, 500} {
		histogram.Observe(value)
	}
	count, sum, min, max := histogram.Snapshot()
	if count != 4 || sum != 605 || min != 5 || max != 500 {
		t.Fatalf("snapshot=%d/%d/%d/%d", count, sum, min, max)
	}
	var decoded struct {
		Count   int64            `json:"count"`
		Sum     int64            `json:"sum"`
		Min     int64            `json:"min"`
		Max     int64            `json:"max"`
		Avg     float64          `json:"avg"`
		Buckets map[string]int64 `json:"buckets"`
	}
	if err := json.Unmarshal([]byte(histogram.String()), &decoded); err != nil {
		t.Fatalf("直方图 String 非法 JSON: %v (%s)", err, histogram.String())
	}
	// 5→桶"10"，50/50→桶"100"，500→溢出桶"+Inf"。
	if decoded.Buckets["10"] != 1 || decoded.Buckets["100"] != 2 || decoded.Buckets["+Inf"] != 1 {
		t.Fatalf("桶分布错误: %+v", decoded.Buckets)
	}
	if decoded.Count != 4 || decoded.Avg < 151 || decoded.Avg > 152 {
		t.Fatalf("count/avg 错误: %+v", decoded)
	}
}

func TestHistogramEmptyIsValidJSON(t *testing.T) {
	histogram := NewHistogram("test_hist_empty", nil)
	if !json.Valid([]byte(histogram.String())) {
		t.Fatalf("空直方图 String 非法 JSON: %s", histogram.String())
	}
}

func TestPublishRatioHandlesNaN(t *testing.T) {
	PublishRatio("test_ratio_nan", func() float64 { return math.NaN() })
	// 幂等：重复 Publish 不 panic。
	PublishRatio("test_ratio_nan", func() float64 { return 0.5 })
}

func TestRotatingWriterRotatesAndCapsBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	// maxBytes=100，maxBackups=2：写多行必触发轮转，历史文件不超过 2 个。
	writer, err := newRotatingWriter(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 60) + "\n"
	for i := 0; i < 10; i++ {
		if _, err := writer.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("主日志文件应存在: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("轮转 .1 应存在: %v", err)
	}
	if _, err := os.Stat(path + ".2"); err != nil {
		t.Fatalf("轮转 .2 应存在: %v", err)
	}
	// maxBackups=2：.3 不应存在。
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("超出 maxBackups 的 .3 不应存在")
	}
}

func TestInstallJSONLoggerWritesStructuredRecords(t *testing.T) {
	dir := t.TempDir()
	closer, err := InstallJSONLogger(dir, "unittest", nil)
	if err != nil {
		t.Fatal(err)
	}
	slog.Info("回合结束", "turn_id", "t1", "outcome", "finished", "duration_ms", 123)
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(filepath.Join(dir, "unittest.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	found := false
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("日志行非法 JSON: %v", err)
		}
		if record["msg"] == "回合结束" {
			found = true
			if record["component"] != "unittest" || record["turn_id"] != "t1" || record["outcome"] != "finished" {
				t.Fatalf("结构化字段缺失: %+v", record)
			}
		}
	}
	if !found {
		t.Fatal("未找到结构化回合日志记录")
	}
}
