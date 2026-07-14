//go:build integration

package providers

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
)

func TestDashScopeASRLive(t *testing.T) {
	if os.Getenv("RUSHES_REQUIRE_LIVE_MODELS") != "1" {
		t.Skip("设置 RUSHES_REQUIRE_LIVE_MODELS=1 才运行真实 DashScope ASR")
	}
	key := strings.TrimSpace(os.Getenv("RUSHES_DASHSCOPE_API_KEY"))
	path := strings.TrimSpace(os.Getenv("RUSHES_ASR_LIVE_AUDIO"))
	if key == "" || path == "" {
		t.Fatal("真实 ASR 测试需要 RUSHES_DASHSCOPE_API_KEY 和 RUSHES_ASR_LIVE_AUDIO")
	}
	modelName := strings.TrimSpace(os.Getenv("RUSHES_DASHSCOPE_ASR_MODEL"))
	recognizer, err := NewDashScopeASR(DashScopeASRConfig{
		APIKey: key, BaseURL: os.Getenv("RUSHES_DASHSCOPE_ASR_BASE_URL"), Model: modelName,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := recognizer.Recognize(t.Context(), contracts.SpeechRecognitionRequest{
		AudioPath: path, Language: "zh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Text) == "" || result.ProviderID != DefaultASRModel {
		t.Fatalf("provider=%q text_runes=%d", result.ProviderID, utf8.RuneCountInString(result.Text))
	}
	wordCount := 0
	for _, segment := range result.Segments {
		wordCount += len(segment.Words)
	}
	if len(result.Segments) == 0 || wordCount == 0 {
		t.Fatalf("provider=%q 缺少词级时间戳 segments=%d words=%d", result.ProviderID, len(result.Segments), wordCount)
	}
	t.Logf(
		"DASHSCOPE_ASR_LIVE_OK provider=%s text_runes=%d timestamp_segments=%d timestamp_words=%d",
		result.ProviderID, utf8.RuneCountInString(result.Text), len(result.Segments), wordCount,
	)
}
