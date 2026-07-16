package understanding_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/providers"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

func TestLivePreviewInspectionFindsUnrelatedBroll(t *testing.T) {
	if os.Getenv("RUSHES_REQUIRE_LIVE_MODELS") != "1" {
		t.Skip("设置 RUSHES_REQUIRE_LIVE_MODELS=1 才运行真实预览视觉检查")
	}
	fixture := strings.TrimSpace(os.Getenv("RUSHES_PREVIEW_VLM_FIXTURE"))
	if fixture == "" {
		t.Skip("设置 RUSHES_PREVIEW_VLM_FIXTURE 才运行真实错误 B-roll 用例")
	}
	if _, err := os.Stat(fixture); err != nil {
		t.Fatalf("预览 fixture 不可读: %v", err)
	}
	key := strings.TrimSpace(os.Getenv("RUSHES_DASHSCOPE_API_KEY"))
	if key == "" {
		t.Fatal("真实预览视觉检查缺少 RUSHES_DASHSCOPE_API_KEY")
	}
	tiers, err := providers.NewQwenTiers(t.Context(), providers.QwenTierConfig{
		APIKey: key, BaseURL: os.Getenv("RUSHES_DASHSCOPE_BASE_URL"),
		VisionModel: os.Getenv("RUSHES_QWEN_VISION_MODEL"),
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := storage.NewPaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Initialize(); err != nil {
		t.Fatal(err)
	}
	document := timeline.Empty("draft_live_preview", 1)
	document.DurationFrames = 90
	document.Tracks[0].Clips = []timeline.Clip{{
		TimelineClipID: "clip_primary", TrackID: "visual_base",
		TimelineStartFrame: 0, TimelineEndFrame: 90,
	}}
	document.Tracks[1].Clips = []timeline.Clip{{
		TimelineClipID: "clip_wrong_broll", TrackID: "visual_overlay",
		TimelineStartFrame: 0, TimelineEndFrame: 90,
	}}
	result, err := understanding.NewAnalyzer(tiers.Vision).InspectPreview(
		t.Context(), paths, fixture, document,
		map[int]string{45: "B-roll 用途：展示笔记本电脑键盘右上角的指纹解锁按键"},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf(
		"PREVIEW_VLM_RESULT latency_ms=%d frame_count=%d prompt_tokens=%d total_tokens=%d findings=%#v",
		result.LatencyMS, result.FrameCount, result.PromptTokens, result.TotalTokens, result.Findings,
	)
	found := false
	for _, finding := range result.Findings {
		if finding.Check == "visual_broll_mismatch" && slices.Contains(finding.Frames, 45) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("真实视觉检查未指出错误 B-roll 中点: %#v", result)
	}
}
