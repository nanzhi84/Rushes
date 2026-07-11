package providers

import (
	"net"
	"net/http"
	"testing"
	"time"
)

func TestNewIPv4Client(t *testing.T) {
	t.Parallel()

	client := NewIPv4Client(17 * time.Second)
	if client.Timeout != 17*time.Second {
		t.Fatalf("Timeout = %s", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型 = %T", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("provider client 不得读取系统代理")
	}
	if transport.DialContext == nil {
		t.Fatal("缺少强制 IPv4 的 DialContext")
	}
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("应启用 HTTP/2 协商")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	accepted := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = connection.Close()
		}
		close(accepted)
	}()
	connection, err := transport.DialContext(t.Context(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	<-accepted
}

func TestProviderConfigValidation(t *testing.T) {
	t.Parallel()

	if _, err := NewQwen(t.Context(), QwenConfig{}); err == nil {
		t.Fatal("缺少 qwen key 应失败")
	}
	if _, err := NewArk(t.Context(), ArkConfig{}); err == nil {
		t.Fatal("缺少 ark 凭据应失败")
	}
	if _, err := NewArk(t.Context(), ArkConfig{APIKey: "x"}); err == nil {
		t.Fatal("缺少 ark model 应失败")
	}
	if _, err := NewArk(t.Context(), ArkConfig{APIKey: "x", Model: "ep", Retries: -1}); err == nil {
		t.Fatal("负重试次数应失败")
	}
}

func TestQwenThreeTierAssembly(t *testing.T) {
	t.Parallel()
	tiers, err := NewQwenTiers(t.Context(), QwenTierConfig{APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	if tiers.Planner == nil || tiers.Chat == nil || tiers.Vision == nil {
		t.Fatalf("tiers=%#v", tiers)
	}
}

func TestProviderValidCustomAndDefaultAssemblies(t *testing.T) {
	t.Parallel()
	if model, err := NewQwen(t.Context(), QwenConfig{
		APIKey: "key", BaseURL: "https://example.com/v1", Model: "custom", Timeout: time.Second,
		EnableThinking: true,
	}); err != nil || model == nil {
		t.Fatalf("qwen=%T err=%v", model, err)
	}
	if model, err := NewQwen(t.Context(), QwenConfig{APIKey: "key"}); err != nil || model == nil {
		t.Fatalf("default qwen=%T err=%v", model, err)
	}
	for _, config := range []ArkConfig{
		{APIKey: "key", Model: "endpoint"},
		{AccessKey: "ak", SecretKey: "sk", Model: "endpoint", Timeout: time.Second, Retries: 2},
	} {
		if model, err := NewArk(t.Context(), config); err != nil || model == nil {
			t.Fatalf("ark=%T config=%#v err=%v", model, config, err)
		}
	}
	if _, err := NewQwenTiers(t.Context(), QwenTierConfig{}); err == nil {
		t.Fatal("tiers without key should fail")
	}
}
