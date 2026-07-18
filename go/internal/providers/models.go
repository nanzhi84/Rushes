package providers

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/model/ark"
	"github.com/cloudwego/eino-ext/components/model/qwen"
	"github.com/cloudwego/eino/components/model"
)

const (
	DefaultDashScopeBaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	DefaultChatModel        = "qwen3.7-max"
	DefaultVisionModel      = "qwen3.7-plus"
	defaultAgentTimeout     = 120 * time.Second
)

type QwenConfig struct {
	APIKey         string
	BaseURL        string
	Model          string
	Timeout        time.Duration
	EnableThinking bool
}

type QwenTierConfig struct {
	APIKey      string
	BaseURL     string
	ChatModel   string
	VisionModel string
}

// ModelTiers 是与厂商无关的聊天/视觉双档模型集合，供 API/worker 装配层消费。
type ModelTiers struct {
	Chat   model.ToolCallingChatModel
	Vision model.ToolCallingChatModel
}

func NewQwenTiers(ctx context.Context, config QwenTierConfig) (ModelTiers, error) {
	if config.ChatModel == "" {
		config.ChatModel = DefaultChatModel
	}
	if config.VisionModel == "" {
		config.VisionModel = DefaultVisionModel
	}
	chat, err := NewQwen(ctx, QwenConfig{
		APIKey: config.APIKey, BaseURL: config.BaseURL, Model: config.ChatModel,
		Timeout: defaultAgentTimeout,
	})
	if err != nil {
		return ModelTiers{}, err
	}
	vision, err := NewQwen(ctx, QwenConfig{
		APIKey: config.APIKey, BaseURL: config.BaseURL, Model: config.VisionModel,
		Timeout: 180 * time.Second,
	})
	if err != nil {
		return ModelTiers{}, err
	}
	return ModelTiers{Chat: chat, Vision: vision}, nil
}

func NewQwen(ctx context.Context, cfg QwenConfig) (model.ToolCallingChatModel, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("缺少 DashScope API 密钥")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultDashScopeBaseURL
	}
	if cfg.Model == "" {
		cfg.Model = DefaultChatModel
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}

	return qwen.NewChatModel(ctx, &qwen.ChatModelConfig{
		APIKey:         cfg.APIKey,
		BaseURL:        cfg.BaseURL,
		Model:          cfg.Model,
		HTTPClient:     NewIPv4Client(cfg.Timeout),
		EnableThinking: &cfg.EnableThinking,
	})
}

type ArkConfig struct {
	APIKey    string
	AccessKey string
	SecretKey string
	BaseURL   string
	Region    string
	Model     string
	Timeout   time.Duration
	Retries   int
}

func NewArk(ctx context.Context, cfg ArkConfig) (model.ToolCallingChatModel, error) {
	if strings.TrimSpace(cfg.APIKey) == "" &&
		(strings.TrimSpace(cfg.AccessKey) == "" || strings.TrimSpace(cfg.SecretKey) == "") {
		return nil, errors.New("缺少 Ark APIKey 或 AK/SK")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("缺少 Ark Model ID 或推理接入点 ID")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.Retries < 0 {
		return nil, errors.New("ark 重试次数不能为负数")
	}

	retries := cfg.Retries
	return ark.NewChatModel(ctx, &ark.ChatModelConfig{
		APIKey:    cfg.APIKey,
		AccessKey: cfg.AccessKey,
		SecretKey: cfg.SecretKey,
		BaseURL:   cfg.BaseURL,
		Region:    cfg.Region,
		Model:     cfg.Model,
		HTTPClient: NewIPv4Client(
			cfg.Timeout,
		),
		RetryTimes: &retries,
	})
}

// ArkTierConfig 描述火山方舟聊天/视觉双档装配所需配置；密钥支持 APIKey 或 AK/SK。
type ArkTierConfig struct {
	APIKey      string
	AccessKey   string
	SecretKey   string
	BaseURL     string
	Region      string
	ChatModel   string
	VisionModel string
	Retries     int
}

// NewArkTiers 按与 DashScope 相同的超时约定装配火山方舟聊天(120s)/视觉(180s)双档。
// 缺少密钥或模型 ID 时由 NewArk 在构造前返回简体中文错误（不含任何密钥值）。
func NewArkTiers(ctx context.Context, config ArkTierConfig) (ModelTiers, error) {
	chat, err := NewArk(ctx, ArkConfig{
		APIKey: config.APIKey, AccessKey: config.AccessKey, SecretKey: config.SecretKey,
		BaseURL: config.BaseURL, Region: config.Region, Model: config.ChatModel,
		Timeout: defaultAgentTimeout, Retries: config.Retries,
	})
	if err != nil {
		return ModelTiers{}, err
	}
	vision, err := NewArk(ctx, ArkConfig{
		APIKey: config.APIKey, AccessKey: config.AccessKey, SecretKey: config.SecretKey,
		BaseURL: config.BaseURL, Region: config.Region, Model: config.VisionModel,
		Timeout: 180 * time.Second, Retries: config.Retries,
	})
	if err != nil {
		return ModelTiers{}, err
	}
	return ModelTiers{Chat: chat, Vision: vision}, nil
}
