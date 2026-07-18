package config

import (
	"strings"
	"testing"
)

func TestResolveChatProviderDefaultsAndArk(t *testing.T) {
	cases := map[string]ChatProvider{
		"":          ProviderDashScope,
		"   ":       ProviderDashScope,
		"dashscope": ProviderDashScope,
		"DashScope": ProviderDashScope,
		"  ark  ":   ProviderArk,
		"ark":       ProviderArk,
		"ARK":       ProviderArk,
	}
	for raw, want := range cases {
		got, err := ResolveChatProvider(raw)
		if err != nil {
			t.Fatalf("ResolveChatProvider(%q) 意外报错：%v", raw, err)
		}
		if got != want {
			t.Fatalf("ResolveChatProvider(%q)=%q，期望 %q", raw, got, want)
		}
	}
}

func TestResolveChatProviderRejectsUnknown(t *testing.T) {
	_, err := ResolveChatProvider("openai")
	if err == nil {
		t.Fatal("非法厂商值应报错")
	}
	message := err.Error()
	for _, must := range []string{EnvChatProvider, string(ProviderDashScope), string(ProviderArk)} {
		if !strings.Contains(message, must) {
			t.Fatalf("错误信息 %q 缺少 %q", message, must)
		}
	}
}
