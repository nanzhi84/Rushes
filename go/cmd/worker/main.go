package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/config"
	"github.com/nanzhi84/Rushes/go/internal/providers"
	"github.com/nanzhi84/Rushes/go/internal/storage"
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("Rushes Go worker ready", "concurrency", concurrency, "kinds", registry.Kinds())
	err = runner.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
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
