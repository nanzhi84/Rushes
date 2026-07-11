package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agent"
	"github.com/nanzhi84/Rushes/go/internal/api"
	"github.com/nanzhi84/Rushes/go/internal/config"
	"github.com/nanzhi84/Rushes/go/internal/providers"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func main() {
	if err := run(); err != nil {
		slog.Error("Rushes API 退出", "error", err)
		os.Exit(1)
	}
}

func run() error {
	repoRoot := defaultRepoRoot()
	envPath := flag.String("env-file", filepath.Join(repoRoot, ".env"), "dotenv 文件路径")
	workspaceFlag := flag.String("workspace", "", "本地工作区目录")
	portFlag := flag.Int("port", 0, "监听端口")
	tokenFlag := flag.String("token", "", "API Bearer token")
	flag.Parse()
	if err := config.LoadDotEnv(*envPath); err != nil {
		return err
	}
	workspace := firstNonEmpty(*workspaceFlag, os.Getenv("RUSHES_WORKSPACE_PATH"), filepath.Join(repoRoot, ".rushes"))
	port := *portFlag
	if port == 0 {
		port = envInt("RUSHES_API_PORT", 8000)
	}
	token := firstNonEmpty(*tokenFlag, os.Getenv("RUSHES_API_TOKEN"))
	if token == "" {
		token = api.GenerateToken()
	}
	database, err := storage.Open(context.Background(), workspace)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := database.Close(); closeErr != nil {
			slog.Error("关闭数据库失败", "error", closeErr)
		}
	}()
	var tiers providers.QwenTiers
	if key := os.Getenv("RUSHES_DASHSCOPE_API_KEY"); key != "" {
		tiers, err = providers.NewQwenTiers(context.Background(), providers.QwenTierConfig{
			APIKey: key, BaseURL: os.Getenv("RUSHES_DASHSCOPE_BASE_URL"),
			PlannerModel: os.Getenv("RUSHES_QWEN_PLANNER_MODEL"),
			ChatModel:    os.Getenv("RUSHES_QWEN_CHAT_MODEL"),
			VisionModel:  os.Getenv("RUSHES_QWEN_VISION_MODEL"),
		})
		if err != nil {
			return err
		}
	}
	agentService, err := agent.NewServiceWithModels(context.Background(), database, tiers.Chat, tiers.Vision)
	if err != nil {
		return err
	}
	defer agentService.Close()

	server, err := api.NewServer(api.Config{
		Database: database, Token: token, Port: port,
		FSRoots: filepath.SplitList(os.Getenv("RUSHES_FS_ROOTS")),
		Agent:   agentService,
	})
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", port),
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errChannel := make(chan error, 1)
	go func() {
		slog.Info("Rushes Go API ready", "url", fmt.Sprintf("http://127.0.0.1:%d/#t=%s", port, token))
		errChannel <- httpServer.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
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
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
