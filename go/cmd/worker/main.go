package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/config"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/providers"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/telemetry"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
	"github.com/nanzhi84/Rushes/go/internal/worker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("Rushes worker 退出", "error", err)
		os.Exit(1)
	}
}

func run() error {
	repoRoot := defaultRepoRoot()
	envPath := flag.String("env-file", filepath.Join(repoRoot, ".env"), "dotenv 文件路径")
	workspaceFlag := flag.String("workspace", "", "本地工作区目录")
	concurrencyFlag := flag.Int("concurrency", 0, "并发 job 数")
	flag.Parse()
	if err := config.LoadDotEnv(*envPath); err != nil {
		return err
	}
	workspace := firstNonEmpty(*workspaceFlag, os.Getenv("RUSHES_WORKSPACE_PATH"), filepath.Join(repoRoot, ".rushes"))
	concurrency := *concurrencyFlag
	if concurrency <= 0 {
		concurrency = envInt("RUSHES_WORKER_CONCURRENCY", 2)
	}
	database, err := storage.Open(context.Background(), workspace)
	if err != nil {
		return err
	}
	// H3：worker 结构化 JSON 日志落盘到 workspace logs/worker.log，同时镜像 stderr（同 api）。
	logCloser, err := telemetry.InstallJSONLogger(database.Paths.Logs, "worker", os.Stderr)
	if err != nil {
		return err
	}
	defer func() { _ = logCloser.Close() }()
	media.ConfigureFFmpegSandbox(
		[]string{database.Paths.Objects, database.Paths.Temporary, database.Paths.Segments, database.Paths.Cache},
		[]string{database.Paths.Temporary},
	)
	defer func() { _ = database.Close() }()
	registry := worker.NewRegistry()
	if err := worker.RegisterIngest(registry, database); err != nil {
		return err
	}
	if err := worker.RegisterRender(registry, database); err != nil {
		return err
	}
	provider, err := config.ResolveChatProvider(os.Getenv(config.EnvChatProvider))
	if err != nil {
		return err
	}
	var analyzer = understanding.NewAnalyzer(nil)
	switch provider {
	case config.ProviderDashScope:
		if key := os.Getenv("RUSHES_DASHSCOPE_API_KEY"); key != "" {
			vision, modelErr := providers.NewQwen(context.Background(), providers.QwenConfig{
				APIKey: key, BaseURL: os.Getenv("RUSHES_DASHSCOPE_BASE_URL"),
				Model: os.Getenv("RUSHES_QWEN_VISION_MODEL"), Timeout: 180 * time.Second,
			})
			if modelErr != nil {
				return modelErr
			}
			analyzer = understanding.NewAnalyzer(vision)
		}
	case config.ProviderArk:
		// worker 只需视觉档；选择 ark 时缺密钥/模型由 NewArk 在启动期报错，不静默降级。
		vision, modelErr := providers.NewArk(context.Background(), providers.ArkConfig{
			APIKey:    os.Getenv("RUSHES_ARK_API_KEY"),
			AccessKey: os.Getenv("RUSHES_ARK_ACCESS_KEY"),
			SecretKey: os.Getenv("RUSHES_ARK_SECRET_KEY"),
			BaseURL:   os.Getenv("RUSHES_ARK_BASE_URL"),
			Region:    os.Getenv("RUSHES_ARK_REGION"),
			Model:     os.Getenv("RUSHES_ARK_VISION_MODEL"),
			Timeout:   180 * time.Second,
		})
		if modelErr != nil {
			return modelErr
		}
		analyzer = understanding.NewAnalyzer(vision)
	}
	if err := worker.RegisterUnderstand(registry, database, analyzer); err != nil {
		return err
	}
	if err := registry.ValidateCatalog(); err != nil {
		return err
	}
	runner, err := worker.NewRunner(worker.RunnerConfig{
		Database: database, Registry: registry, Concurrency: concurrency,
	})
	if err != nil {
		return err
	}
	// H3：worker 是独立进程，度量在本进程 expvar 里；设了 RUSHES_WORKER_METRICS_ADDR 才起一个
	// 最小 HTTP 端点把它们（含 worker_job_* 延迟）暴露到 /debug/metrics。默认不起，避免 CI/测试
	// 抢端口。dev_all.sh 会设它。
	if addr := os.Getenv("RUSHES_WORKER_METRICS_ADDR"); addr != "" {
		metricsServer := startDebugMetricsServer(addr)
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = metricsServer.Shutdown(shutdownCtx)
		}()
		slog.Info("worker 度量端点已监听", "addr", addr, "path", "/debug/metrics")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("Rushes Go worker ready", "concurrency", concurrency, "kinds", registry.Kinds())
	err = runner.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// startDebugMetricsServer 起一个只服务 /debug/metrics 与 /healthz 的最小 HTTP 服务，用于把
// worker 进程的 expvar 度量暴露出去（#95 H3）。监听失败只记日志、不影响 job 处理。
func startDebugMetricsServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/debug/metrics", telemetry.Handler())
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"status":"ok"}`))
	})
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("worker 度量端点异常", "error", err)
		}
	}()
	return server
}

func defaultRepoRoot() string {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return "."
	}
	if filepath.Base(workingDirectory) == "go" {
		return filepath.Dir(workingDirectory)
	}
	return workingDirectory
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
