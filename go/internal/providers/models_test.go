package providers

import "testing"

// NewArkTiers 的装配选择/校验逻辑必须确定性可测，且不触达真实网络：
// 缺密钥、缺聊天模型、缺视觉模型都在构造前 fail fast，齐全时装配双档。

func TestNewArkTiersRequiresCredential(t *testing.T) {
	_, err := NewArkTiers(t.Context(), ArkTierConfig{ChatModel: "chat-ep", VisionModel: "vision-ep"})
	if err == nil {
		t.Fatal("缺少 ark 密钥应报错")
	}
}

func TestNewArkTiersRequiresChatModel(t *testing.T) {
	_, err := NewArkTiers(t.Context(), ArkTierConfig{APIKey: "test-key", VisionModel: "vision-ep"})
	if err == nil {
		t.Fatal("缺少 ark 聊天模型应报错")
	}
}

func TestNewArkTiersRequiresVisionModel(t *testing.T) {
	_, err := NewArkTiers(t.Context(), ArkTierConfig{APIKey: "test-key", ChatModel: "chat-ep"})
	if err == nil {
		t.Fatal("缺少 ark 视觉模型应报错")
	}
}

func TestNewArkTiersBuildsBothTiers(t *testing.T) {
	tiers, err := NewArkTiers(t.Context(), ArkTierConfig{
		APIKey: "test-key", ChatModel: "chat-ep", VisionModel: "vision-ep",
	})
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	if tiers.Chat == nil || tiers.Vision == nil {
		t.Fatal("应同时装配 chat 与 vision")
	}
}
