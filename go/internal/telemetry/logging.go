package telemetry

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

const (
	// defaultMaxLogBytes 是单个日志文件轮转前的大小上限（16 MiB）。
	defaultMaxLogBytes = 16 << 20
	// defaultMaxLogBackups 是保留的历史轮转文件个数（<component>.log.1 … .N）。
	defaultMaxLogBackups = 5
)

// InstallJSONLogger 把默认 slog 换成写入 dir/<component>.log 的结构化 JSON 处理器（按大小
// 轮转），并附带固定 component 字段。alsoStderr 非空时同时镜像到该 writer（dev 下传
// os.Stderr，让终端仍能看到日志）。返回的 io.Closer 关闭底层文件。
//
// 安全约定：本函数只负责落盘管道，不做内容脱敏；调用方（各 slog.* 点）必须自行保证不打印
// 密钥与用户素材全文——落盘只是把既有 slog 记录持久化，不改变其内容（#95 H3 验收）。
func InstallJSONLogger(dir, component string, alsoStderr io.Writer) (io.Closer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建日志目录 %s: %w", dir, err)
	}
	writer, err := newRotatingWriter(
		filepath.Join(dir, component+".log"), defaultMaxLogBytes, defaultMaxLogBackups,
	)
	if err != nil {
		return nil, err
	}
	var sink io.Writer = writer
	if alsoStderr != nil {
		sink = io.MultiWriter(writer, alsoStderr)
	}
	handler := slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler).With("component", component))
	return writer, nil
}

// rotatingWriter 是纯 Go 的按大小轮转 io.WriteCloser：写满 maxBytes 就把当前文件顺次改名为
// .1/.2…（丢弃最旧的 .maxBackups），重开新文件。零外部依赖，契合本仓零 CGO/精简依赖取向。
type rotatingWriter struct {
	mu         sync.Mutex
	path       string
	maxBytes   int64
	maxBackups int
	file       *os.File
	size       int64
}

func newRotatingWriter(path string, maxBytes int64, maxBackups int) (*rotatingWriter, error) {
	writer := &rotatingWriter{path: path, maxBytes: maxBytes, maxBackups: maxBackups}
	if err := writer.open(); err != nil {
		return nil, err
	}
	return writer, nil
}

func (w *rotatingWriter) open() error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("打开日志文件 %s: %w", w.path, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func (w *rotatingWriter) Write(payload []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.maxBytes > 0 && w.size+int64(len(payload)) > w.maxBytes && w.size > 0 {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	written, err := w.file.Write(payload)
	w.size += int64(written)
	return written, err
}

// rotate 关闭当前文件，把 .N-1→.N（丢弃最旧），path→.1，再重开新 path。
func (w *rotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	if w.maxBackups > 0 {
		oldest := fmt.Sprintf("%s.%d", w.path, w.maxBackups)
		_ = os.Remove(oldest)
		for index := w.maxBackups - 1; index >= 1; index-- {
			_ = os.Rename(fmt.Sprintf("%s.%d", w.path, index), fmt.Sprintf("%s.%d", w.path, index+1))
		}
		_ = os.Rename(w.path, w.path+".1")
	}
	return w.open()
}

// Close 关闭底层文件。
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	return w.file.Close()
}
