package understanding

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func TestSuggestVisualRoleUsesUnderstandingThenMaterialOrganization(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		filename   string
		relDir     string
		understood string
		want       string
	}{
		{name: "vlm wins", filename: "产品展示.mp4", relDir: "Broll", understood: "a_roll", want: "a_roll"},
		{name: "a roll directory", filename: "take-01.mp4", relDir: "第二节课实操-口播", want: "a_roll"},
		{name: "b roll directory", filename: "take-01.mp4", relDir: "Broll/键盘", want: "b_roll"},
		{name: "descriptive b roll", filename: "触控板细节.mp4", want: "b_roll"},
		{name: "unknown", filename: "IMG_001.mp4", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := SuggestVisualRole(test.filename, test.relDir, test.understood); got != test.want {
				t.Fatalf("role=%q want=%q", got, test.want)
			}
		})
	}
}

type visionModel struct {
	parts int
}

type failingVisionModel struct{ visionModel }

type scriptedVisionModel struct {
	responses []string
	calls     int
	parts     []int
}

func (modelValue *scriptedVisionModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return modelValue, nil
}

func (modelValue *scriptedVisionModel) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	if len(messages) != 1 {
		return nil, errors.New("VLM 输入消息数量错误")
	}
	modelValue.parts = append(modelValue.parts, len(messages[0].UserInputMultiContent))
	if modelValue.calls >= len(modelValue.responses) {
		return nil, errors.New("缺少 scripted VLM response")
	}
	response := modelValue.responses[modelValue.calls]
	modelValue.calls++
	return schema.AssistantMessage(response, nil), nil
}

func (modelValue *scriptedVisionModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := modelValue.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (modelValue *failingVisionModel) Generate(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.Message, error) {
	return nil, errors.New("vision failed")
}

func (modelValue *visionModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return modelValue, nil
}

func (modelValue *visionModel) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	if len(messages) != 1 {
		return nil, errors.New("VLM 输入消息数量错误")
	}
	modelValue.parts = len(messages[0].UserInputMultiContent)
	return schema.AssistantMessage("人物在室内展示产品，画面稳定。", nil), nil
}

func (modelValue *visionModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := modelValue.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestMiniLoopExtractsFramesCallsVLMAndEmitsDegradedSummary(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	source := filepath.Join(database.Paths.Temporary, "understand.mp4")
	if _, err := media.RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30:duration=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", source); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(source)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,mtime,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES('asset','reference',?,'video','local_path','u.mp4','hash',?,?,'{"duration_sec":1}','ready','none',1)`,
		source, info.ModTime().UnixNano(), info.Size()); err != nil {
		t.Fatal(err)
	}
	asset, err := storage.GetAsset(t.Context(), database.Read(), "asset")
	if err != nil {
		t.Fatal(err)
	}
	vision := &visionModel{}
	notes := []string{}
	summary, err := NewAnalyzer(vision).AnalyzeWithOptions(t.Context(), database, asset, AnalyzeOptions{Focus: "产品"}, func(note string) {
		notes = append(notes, note)
	})
	if err != nil {
		t.Fatal(err)
	}
	if vision.parts != 3 || summary.Version != 2 || summary.Overall != "人物在室内展示产品，画面稳定。" ||
		len(summary.Segments) != 1 || summary.Segments[0].Description != summary.Overall ||
		summary.Segments[0].SourceEndFrame != 30 || summary.AnalysisMethod == "" ||
		len(summary.Degraded) != 1 || len(notes) < 5 {
		t.Fatalf("parts=%d summary=%#v notes=%#v", vision.parts, summary, notes)
	}
}

func TestVideoAnalysisVerifiesCutsAndDescribesEverySegment(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	source := filepath.Join(database.Paths.Temporary, "verified-cut.mkv")
	if _, err := media.RunCommand(
		t.Context(), "ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=red:s=160x90:r=30:d=1",
		"-f", "lavfi", "-i", "color=c=blue:s=160x90:r=30:d=1",
		"-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0", "-c:v", "ffv1", source,
	); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(source)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,mtime,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES('cut_asset','reference',?,'video','local_path','cut.mkv','cut_hash',?,?,'{"duration_sec":2}','ready','none',1)`,
		source, info.ModTime().UnixNano(), info.Size()); err != nil {
		t.Fatal(err)
	}
	asset, _ := storage.GetAsset(t.Context(), database.Read(), "cut_asset")
	vision := &scriptedVisionModel{responses: []string{
		`{"boundaries":[{"id":"c000","kind":"cut","accept":true,"confidence":0.98}]}`,
		`{"overall":"画面由红色场景切换到蓝色场景。","segments":[{"id":"s000","description":"红色场景","tags":["红色"],"quality":"usable"},{"id":"s001","description":"蓝色场景","tags":["蓝色"],"quality":"usable"}]}`,
	}}
	summary, err := NewAnalyzer(vision).AnalyzeWithOptions(
		t.Context(), database, asset, AnalyzeOptions{Depth: "deep", Focus: "转场"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if vision.calls != 2 || len(vision.parts) != 2 || summary.CandidateCuts != 1 ||
		summary.VerifiedCuts != 1 || len(summary.Segments) != 2 ||
		!summary.Segments[1].BoundaryVerified || summary.Segments[1].BoundaryKind != "visual_cut" ||
		summary.Segments[0].Description != "红色场景" || summary.Segments[1].Description != "蓝色场景" {
		t.Fatalf("vision=%#v summary=%#v", vision, summary)
	}
}

func TestVideoAnalysisDegradesInsteadOfFailingOnVisionProviderError(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	source := filepath.Join(database.Paths.Temporary, "single-shot.mp4")
	if _, err := media.RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=30:duration=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", source); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(source)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,mtime,size,
			probe_json,ingest_status,understanding_status,usable)
		VALUES('degraded_video','reference',?,'video','local_path','single.mp4','single_hash',?,?,'{"duration_sec":1}','ready','none',1)`,
		source, info.ModTime().UnixNano(), info.Size()); err != nil {
		t.Fatal(err)
	}
	asset, _ := storage.GetAsset(t.Context(), database.Read(), "degraded_video")
	summary, err := NewAnalyzer(&failingVisionModel{}).AnalyzeWithOptions(t.Context(), database, asset, AnalyzeOptions{Focus: ""}, nil)
	if err != nil || summary.Model != "deterministic-local" ||
		!containsStringValue(summary.Degraded, "visual_summary_unavailable") {
		t.Fatalf("summary=%#v err=%v", summary, err)
	}
}

func TestVideoSpanBudgetPreservesCutsAndAddsAnalysisWindows(t *testing.T) {
	score := 21.0
	spans := buildVideoSpans(30, []videoBoundary{{TimeSec: 10, Score: score, Verified: true}}, AnalyzeOptions{})
	if len(spans) != 3 || spans[0].StartSec != 0 || spans[0].EndSec != 10 ||
		spans[1].StartSec != 10 || !spans[1].BoundaryVerified ||
		spans[2].BoundaryKind != "analysis_window" || spans[2].EndSec != 30 {
		t.Fatalf("spans=%#v", spans)
	}
	longShot := buildVideoSpans(70, nil, AnalyzeOptions{})
	if len(longShot) != 8 {
		t.Fatalf("scan long shot spans=%#v", longShot)
	}
	for index, span := range longShot {
		if span.ID == "" || span.EndSec <= span.StartSec || span.EndSec-span.StartSec > 12 {
			t.Fatalf("span[%d]=%#v", index, span)
		}
	}
	deep := buildVideoSpans(70, nil, AnalyzeOptions{Depth: "deep"})
	if len(deep) != 16 {
		t.Fatalf("deep spans=%d", len(deep))
	}
}

func TestVideoBoundaryCandidateFilteringAndSpanLimits(t *testing.T) {
	candidates := []media.SceneCandidate{
		{PTSTimeSeconds: 0.05, Score: 99},
		{PTSTimeSeconds: 0.5, Score: 1},
		{PTSTimeSeconds: 1.0, Score: 10},
		{PTSTimeSeconds: 1.5, Score: 5},
		{PTSTimeSeconds: 1.9, Score: 99},
	}
	selected, truncated := selectBoundaryCandidates(candidates, 2, 2)
	if !truncated || len(selected) != 2 || selected[0].PTSTimeSeconds != 1 ||
		selected[1].PTSTimeSeconds != 1.5 {
		t.Fatalf("selected=%#v truncated=%v", selected, truncated)
	}
	all, truncated := selectBoundaryCandidates(candidates, 2, 0)
	if truncated || len(all) != 3 {
		t.Fatalf("all=%#v truncated=%v", all, truncated)
	}
	unverified := unverifiedBoundaries(all)
	if len(unverified) != 3 || unverified[1].Verified || unverified[1].Score != 10 {
		t.Fatalf("unverified=%#v", unverified)
	}

	// 相距不足最小阈值的候选保留高分项；max_steps 会限制最终段数。
	spans := buildVideoSpans(4, []videoBoundary{
		{TimeSec: 2, Score: 2},
		{TimeSec: 1, Score: 1},
		{TimeSec: 1.05, Score: 9},
		{TimeSec: 3, Score: 3},
	}, AnalyzeOptions{MaxStepsPerAsset: 2})
	if len(spans) != 2 || spans[0].EndSec != 1.05 || spans[1].BoundaryKind != "visual_cut" {
		t.Fatalf("limited spans=%#v", spans)
	}
	for _, item := range []struct {
		options AnalyzeOptions
		count   int
		window  float64
	}{
		{AnalyzeOptions{MaxStepsPerAsset: 1}, 1, 6},
		{AnalyzeOptions{MaxStepsPerAsset: 99}, 24, 6},
		{AnalyzeOptions{Depth: " DEEP "}, 16, 6},
		{AnalyzeOptions{}, 8, 12},
	} {
		count, window := videoAnalysisBudget(item.options)
		if count != item.count || window != item.window {
			t.Fatalf("options=%#v count=%d window=%v", item.options, count, window)
		}
	}
	segments := segmentsFromSpans([]videoSpan{{
		ID: "s000", StartSec: 0.1, EndSec: 0.1, BoundaryKind: "video_start",
	}})
	if len(segments) != 1 || segments[0].SourceStartFrame != 3 || segments[0].SourceEndFrame != 4 {
		t.Fatalf("segments=%#v", segments)
	}
}

func TestBoundaryVerificationRejectsFlashAndKeepsMissingDecision(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	paths, err := storage.NewPaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Initialize(); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(paths.Temporary, "boundary-source.mp4")
	if _, err := media.RunCommand(
		t.Context(), "ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=30:duration=2",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", source,
	); err != nil {
		t.Fatal(err)
	}
	vision := &scriptedVisionModel{responses: []string{
		`{"boundaries":[{"id":"c000","kind":"flash","accept":true,"confidence":0.99},{"id":"c001","kind":"cut","accept":true,"confidence":0.40}]}`,
	}}
	verified, err := NewAnalyzer(vision).verifySceneCandidates(
		t.Context(), paths, source, 2, []media.SceneCandidate{
			{PTSTimeSeconds: 0.5, Score: 8},
			{PTSTimeSeconds: 1.0, Score: 9},
			{PTSTimeSeconds: 1.5, Score: 10},
		}, "排除闪光",
	)
	if err != nil || len(verified) != 1 || verified[0].TimeSec != 1.5 || verified[0].Verified {
		t.Fatalf("verified=%#v err=%v", verified, err)
	}
}

func TestVideoAnalysisDegradesWhenSceneAndFrameExtractionAreUnavailable(t *testing.T) {
	paths, err := storage.NewPaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Initialize(); err != nil {
		t.Fatal(err)
	}
	result, err := NewAnalyzer(nil).analyzeVideo(
		t.Context(), paths, filepath.Join(paths.Temporary, "missing.mp4"), 1,
		AnalyzeOptions{MaxStepsPerAsset: 1}, func(string) {},
	)
	if err != nil || len(result.Segments) != 1 ||
		!containsStringValue(result.Degraded, "scene_detection_unavailable") ||
		!containsStringValue(result.Degraded, "representative_frame_extract_partial") ||
		!containsStringValue(result.Degraded, "visual_understanding_unavailable") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestStructuredVisionHelpersAreDefensive(t *testing.T) {
	result := videoAnalysisResult{Overall: "old", Segments: []Segment{{Description: "old", Quality: "usable"}}}
	applySegmentDescriptions(&result, "```json\n{\"overall\":\"new\",\"segments\":[{\"id\":\"s000\",\"description\":\"detail\",\"tags\":[\"fire\",\"fire\"],\"quality\":\"dark\"}]}\n```")
	if result.Overall != "new" || result.Segments[0].Description != "detail" ||
		result.Segments[0].Quality != "dark" || len(result.Segments[0].Tags) != 2 {
		t.Fatalf("result=%#v", result)
	}
	plain := videoAnalysisResult{Segments: []Segment{{Description: "old"}}}
	applySegmentDescriptions(&plain, "plain summary")
	if plain.Overall != "plain summary" || plain.Segments[0].Description != "plain summary" {
		t.Fatalf("plain=%#v", plain)
	}
	if normalizeVisualQuality("unknown") != "usable" {
		t.Fatal("normalization mismatch")
	}
}

func containsStringValue(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func TestMiniLoopHonorsCancelledContext(t *testing.T) {
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = NewAnalyzer(nil).AnalyzeWithOptions(ctx, database, storage.Asset{ID: "missing"}, AnalyzeOptions{Focus: ""}, nil)
	if err == nil {
		t.Fatal("取消 context 应终止理解")
	}
}

func TestStaticAndAudioKindsDegradeDeterministically(t *testing.T) {
	t.Parallel()
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	for _, item := range []struct {
		id       string
		kind     string
		expected string
	}{
		{"image", "image", "still"}, {"audio", "audio", "sfx"}, {"font", "font", "visual"},
	} {
		path := filepath.Join(database.Paths.Temporary, item.id+".bin")
		if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
				probe_json,ingest_status,understanding_status,usable)
			VALUES(?, 'reference', ?, ?, 'local_path', ?, ?, 7, '{"duration_sec":1}', 'ready', 'none', 1)`,
			item.id, path, item.kind, item.id, item.id); err != nil {
			t.Fatal(err)
		}
		asset, _ := storage.GetAsset(t.Context(), database.Read(), item.id)
		summary, err := NewAnalyzer(nil).AnalyzeWithOptions(t.Context(), database, asset, AnalyzeOptions{Focus: ""}, nil)
		if err != nil || summary.SemanticRole != item.expected || summary.Model != "deterministic-local" || summary.Segments[0].EndSec != 1 {
			t.Fatalf("item=%s summary=%#v err=%v", item.id, summary, err)
		}
	}

	image, _ := storage.GetAsset(t.Context(), database.Read(), "image")
	if _, err := NewAnalyzer(&failingVisionModel{}).AnalyzeWithOptions(t.Context(), database, image, AnalyzeOptions{Focus: "失败"}, nil); err == nil {
		t.Fatal("vision failure should propagate")
	}
	if _, err := extractFrames(t.Context(), database.Paths, "/missing", "image", 1); err == nil {
		t.Fatal("missing image should fail")
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := extractFrames(cancelled, database.Paths, "/missing", "video", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}
	for _, kind := range []string{"audio", "font"} {
		frames, err := extractFrames(t.Context(), database.Paths, "/unused", kind, 1)
		if err != nil || frames != nil {
			t.Fatalf("kind=%s frames=%v err=%v", kind, frames, err)
		}
	}
	for _, value := range []any{float64(1), float32(2), 3, "bad"} {
		_ = numeric(value)
	}
	for _, kind := range []string{"video", "audio", "image", "font", "unknown"} {
		_ = kindLabel(kind)
		_ = semanticRole(kind)
	}
	for _, item := range []struct {
		filename string
		duration float64
		role     string
	}{
		{"海浪音效.mp3", 180, "sfx"},
		{"fire-sfx.wav", 60, "sfx"},
		{"电影配乐.wav", 12, "bgm"},
		{"IGNIS.wav", 48, "bgm"},
		{"short.wav", 8, "sfx"},
	} {
		if role := ClassifyAudioRole(item.filename, item.duration); role != item.role {
			t.Fatalf("filename=%s duration=%v role=%s want=%s", item.filename, item.duration, role, item.role)
		}
	}
	if tags := summaryTags("audio", "bgm"); len(tags) != 2 || tags[1] != "bgm" {
		t.Fatalf("tags=%v", tags)
	}
	if stringPointer("") != nil || stringPointer("x") == nil {
		t.Fatal("string pointer mismatch")
	}
}
