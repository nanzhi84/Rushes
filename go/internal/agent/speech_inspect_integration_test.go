//go:build integration

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/providers"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestSpeechInspectBuildsRealFunASRTranscript(t *testing.T) {
	if os.Getenv("RUSHES_REQUIRE_LIVE_MODELS") != "1" {
		t.Skip("设置 RUSHES_REQUIRE_LIVE_MODELS=1 才运行真实 speech.inspect ASR")
	}
	key := strings.TrimSpace(os.Getenv("RUSHES_DASHSCOPE_API_KEY"))
	source := strings.TrimSpace(os.Getenv("RUSHES_ASR_LIVE_SOURCE"))
	if key == "" || source == "" {
		t.Fatal("真实 speech.inspect 测试需要 API key 与 RUSHES_ASR_LIVE_SOURCE")
	}
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_live_fun_asr")
	// 真实素材目录可能带同名 SRT；该验收必须覆盖 DashScope ASR，而不是被
	// sidecar 快路径替代，因此用不同 basename 的符号链接读取同一真实视频。
	linkedSource := filepath.Join(t.TempDir(), "live-aroll-no-sidecar"+filepath.Ext(source))
	if err := os.Symlink(source, linkedSource); err != nil {
		t.Fatal(err)
	}
	insertSpeechFixtureAsset(t, database, "draft_live_fun_asr", "asset_live_aroll", linkedSource)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	recognizer, err := providers.NewDashScopeASR(providers.DashScopeASRConfig{
		APIKey: key, BaseURL: os.Getenv("RUSHES_DASHSCOPE_ASR_BASE_URL"),
		Model: os.Getenv("RUSHES_DASHSCOPE_ASR_MODEL"),
	})
	if err != nil {
		t.Fatal(err)
	}
	service.SetSpeechRecognizer(recognizer)
	result, err := service.toolInspectSpeech(t.Context(), "draft_live_fun_asr", rushestools.SpeechInspectInput{
		AssetID: "asset_live_aroll", Language: "zh", MaxUtterances: 200,
		IncludeWords: true, MaxWords: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.ProviderID, providers.DefaultASRModel+"+") ||
		result.UtteranceTotal < 2 || len(result.Utterances) == 0 || result.WordTotal == 0 ||
		len(result.Utterances[0].Words) == 0 {
		t.Fatalf(
			"provider=%q utterance_total=%d returned=%d word_total=%d",
			result.ProviderID, result.UtteranceTotal, len(result.Utterances), result.WordTotal,
		)
	}
	foundFingerprintWords := false
	for _, utterance := range result.Utterances {
		if !strings.Contains(utterance.Text, "指纹") {
			continue
		}
		_, _, foundFingerprintWords = semanticWordRange(utterance.Words, "指纹", 120)
		break
	}
	if !foundFingerprintWords {
		t.Fatal("真实 ASR 没有为指纹台词提供可定位的连续词级范围")
	}
	const executionRuns = 20
	succeeded := 1
	for run := 1; run < executionRuns; run++ {
		cached, cacheErr := service.toolInspectSpeech(t.Context(), "draft_live_fun_asr", rushestools.SpeechInspectInput{
			AssetID: "asset_live_aroll", Query: result.Utterances[0].Text, MaxUtterances: 5,
		})
		if cacheErr == nil && cached.CacheHit && len(cached.Utterances) > 0 {
			succeeded++
		}
	}
	if float64(succeeded)/float64(executionRuns) < 0.95 {
		t.Fatalf("speech.inspect 实际执行成功率 %d/%d 低于 95%%", succeeded, executionRuns)
	}
	t.Logf(
		"SPEECH_INSPECT_FUN_ASR_OK provider=%s utterances=%d words=%d pauses=%d execution_success=%d/%d",
		result.ProviderID, result.UtteranceTotal, result.WordTotal, len(result.Pauses), succeeded, executionRuns,
	)
}
