package understanding

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

type ProgressFunc func(note string)

type Segment struct {
	StartSec         float64  `json:"start_s"`
	EndSec           float64  `json:"end_s"`
	SourceStartFrame int      `json:"source_start_frame"`
	SourceEndFrame   int      `json:"source_end_frame"`
	Description      string   `json:"description"`
	Transcript       *string  `json:"transcript"`
	Tags             []string `json:"tags"`
	Quality          string   `json:"quality"`
	Notes            *string  `json:"notes"`
	BoundaryKind     string   `json:"boundary_kind,omitempty"`
	BoundaryScore    *float64 `json:"boundary_score,omitempty"`
	BoundaryVerified bool     `json:"boundary_verified,omitempty"`
	Subjects         []string `json:"subjects,omitempty"`
	Actions          []string `json:"actions,omitempty"`
	Setting          []string `json:"setting,omitempty"`
	ShotScale        string   `json:"shot_scale,omitempty"`
	Composition      string   `json:"composition,omitempty"`
	Lighting         []string `json:"lighting,omitempty"`
	Mood             []string `json:"mood,omitempty"`
	EditHints        []string `json:"edit_hints,omitempty"`
	OverexposedRatio *float64 `json:"overexposed_ratio,omitempty"`
	SharpnessScore   *float64 `json:"sharpness_score,omitempty"`
}

type Summary struct {
	AssetID        string    `json:"asset_id"`
	Version        int       `json:"version"`
	Focus          *string   `json:"focus"`
	SemanticRole   string    `json:"semantic_role"`
	Overall        string    `json:"overall"`
	Language       *string   `json:"language"`
	Segments       []Segment `json:"segments"`
	GeneratedAt    string    `json:"generated_at"`
	Model          string    `json:"model"`
	AnalysisMethod string    `json:"analysis_method,omitempty"`
	CandidateCuts  int       `json:"candidate_cut_count,omitempty"`
	VerifiedCuts   int       `json:"verified_cut_count,omitempty"`
	Degraded       []string  `json:"degraded,omitempty"`
	AnalysisDepth  string    `json:"analysis_depth,omitempty"`
	AnalysisSteps  int       `json:"analysis_steps,omitempty"`
}

type AnalyzeOptions struct {
	Focus            string
	Depth            string
	MaxStepsPerAsset int
}

type Analyzer struct {
	vision model.ToolCallingChatModel
}

func NewAnalyzer(vision model.ToolCallingChatModel) *Analyzer {
	return &Analyzer{vision: vision}
}

func (analyzer *Analyzer) AnalyzeWithOptions(
	ctx context.Context,
	database *storage.DB,
	asset storage.Asset,
	options AnalyzeOptions,
	progress ProgressFunc,
) (Summary, error) {
	options = NormalizeAnalyzeOptions(asset, options)
	if progress == nil {
		progress = func(string) {}
	}
	source, kind, err := media.ResolveAssetSource(ctx, database, asset.ID)
	if err != nil {
		return Summary{}, err
	}
	duration := numeric(asset.Probe["duration_sec"])
	if duration <= 0 && kind != "image" && kind != "font" {
		probe, probeErr := media.ProbeFile(ctx, source)
		if probeErr != nil {
			return Summary{}, probeErr
		}
		duration = probe.DurationSec
	}
	if duration <= 0 {
		duration = 1
	}
	if kind == "audio" {
		progress("audio_probe：正在识别音频类型与时长")
	} else if kind != "video" {
		progress("view_frames：正在抽取代表帧")
	}
	role := semanticRole(kind)
	overall := fmt.Sprintf("%s 素材，时长约 %.2f 秒。", kindLabel(kind), duration)
	segments := []Segment{{
		StartSec: 0, EndSec: duration,
		SourceStartFrame: 0, SourceEndFrame: max(1, int(duration*30+0.5)),
		Description: overall, Transcript: nil, Tags: summaryTags(kind, role), Quality: "usable",
	}}
	analysisMethod := "deterministic-metadata"
	candidateCuts, verifiedCuts := 0, 0
	additionalDegraded := []string{}
	if kind == "audio" {
		role = ClassifyAudioRole(asset.Filename, duration)
		overall = fmt.Sprintf(
			"音频素材《%s》，时长约 %.2f 秒，建议作为%s使用。",
			asset.Filename,
			duration,
			map[string]string{"bgm": "背景音乐", "sfx": "音效"}[role],
		)
		segments[0].Description = overall
		segments[0].Tags = summaryTags(kind, role)
	}
	modelName := "deterministic-local"
	if kind == "video" {
		videoResult, videoErr := analyzer.analyzeVideo(
			ctx, database.Paths, source, duration, options, progress,
		)
		if videoErr != nil {
			return Summary{}, videoErr
		}
		overall, segments = videoResult.Overall, videoResult.Segments
		role = SuggestVisualRole(asset.Filename, pointerString(asset.RelDir), videoResult.SemanticRole)
		if role == "" {
			role = "visual"
		}
		modelName, analysisMethod = videoResult.Model, videoResult.AnalysisMethod
		candidateCuts, verifiedCuts = videoResult.CandidateCuts, videoResult.VerifiedCuts
		additionalDegraded = append(additionalDegraded, videoResult.Degraded...)
	} else {
		frames, frameErr := extractFrames(ctx, database.Paths, source, kind, duration)
		if frameErr != nil {
			return Summary{}, frameErr
		}
		if analyzer.vision != nil && len(frames) > 0 {
			progress("view_frames：正在调用 VLM 理解画面")
			description, visionErr := analyzer.describeFrames(ctx, frames, options.Focus)
			if visionErr != nil {
				return Summary{}, visionErr
			}
			if description != "" {
				overall = description
				segments[0].Description = description
			}
			modelName = "qwen-vlm"
		}
	}
	progress("transcribe：一期未配置 ASR，保留降级提示")
	note := "音频转写不可用；摘要仅基于画面与媒体元数据。"
	degraded := []string{"transcribe_unavailable"}
	if kind == "audio" {
		note = "音频角色基于文件名与时长识别；未执行语音转写或音色分类。"
		degraded = []string{"audio_content_analysis_unavailable"}
	}
	if kind == "image" || kind == "font" {
		note = "静态素材无需音频转写。"
	}
	degraded = append(degraded, additionalDegraded...)
	for index := range segments {
		if segments[index].Notes == nil {
			segments[index].Notes = &note
		}
	}
	progress("emit_summary：正在生成结构化摘要")
	return Summary{
		AssetID: asset.ID, Version: 2, Focus: stringPointer(options.Focus),
		SemanticRole: role, Overall: overall,
		Segments:    segments,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), Model: modelName,
		AnalysisMethod: analysisMethod, CandidateCuts: candidateCuts, VerifiedCuts: verifiedCuts,
		Degraded: degraded, AnalysisDepth: options.Depth, AnalysisSteps: len(segments),
	}, nil
}

// ClassifyAudioRole 给本地音频一个可解释的初始角色。显式文件名优先，
// 没有语义词时再用短音频偏音效、长音频偏 BGM 的保守规则。
func ClassifyAudioRole(filename string, durationSec float64) string {
	name := strings.ToLower(filename)
	for _, token := range []string{"音效", "环境声", "sfx", "sound effect", "sound-effect", "fx"} {
		if strings.Contains(name, token) {
			return "sfx"
		}
	}
	for _, token := range []string{"背景音乐", "配乐", "音乐", "bgm", "music", "song"} {
		if strings.Contains(name, token) {
			return "bgm"
		}
	}
	if durationSec > 0 && durationSec <= 30 {
		return "sfx"
	}
	return "bgm"
}

// SuggestVisualRole combines explicit visual understanding with the user's
// directory organization. It is only an observable role hint: no editing
// decision is made here. VLM evidence wins; path/filename tokens provide a
// deterministic fallback for well-organized A-roll/B-roll material packs.
func SuggestVisualRole(filename, relDir, understood string) string {
	if role := normalizeVisualRole(understood); role != "" {
		return role
	}
	value := strings.ToLower(strings.Join([]string{relDir, filename}, "/"))
	for _, token := range []string{
		"a-roll", "a_roll", "aroll", "talking head", "talking-head", "口播", "采访", "讲解",
	} {
		if strings.Contains(value, token) {
			return "a_roll"
		}
	}
	for _, token := range []string{
		"b-roll", "b_roll", "broll", "空镜", "展示", "特写", "对比", "演示", "细节",
	} {
		if strings.Contains(value, token) {
			return "b_roll"
		}
	}
	return ""
}

func normalizeVisualRole(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "a_roll", "a-roll", "aroll":
		return "a_roll"
	case "b_roll", "b-roll", "broll":
		return "b_roll"
	default:
		return ""
	}
}

func pointerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func summaryTags(kind, role string) []string {
	if role == "" || role == kind {
		return []string{kind}
	}
	return []string{kind, role}
}

func (analyzer *Analyzer) describeFrames(ctx context.Context, frames [][]byte, focus string) (string, error) {
	prompt := "描述这些视频代表帧的主体、动作、场景、镜头质量和可剪辑价值。只返回一段简洁中文。"
	if focus != "" {
		prompt += "重点关注：" + focus
	}
	parts := []schema.MessageInputPart{{Type: schema.ChatMessagePartTypeText, Text: prompt}}
	for _, frame := range frames {
		encoded := base64.StdEncoding.EncodeToString(frame)
		parts = append(parts, schema.MessageInputPart{
			Type: schema.ChatMessagePartTypeImageURL,
			Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{Base64Data: &encoded, MIMEType: "image/jpeg"},
				Detail:            schema.ImageURLDetailLow,
			},
		})
	}
	response, err := analyzer.vision.Generate(ctx, []*schema.Message{{
		Role: schema.User, UserInputMultiContent: parts,
	}})
	if err != nil {
		return "", err
	}
	return response.Content, nil
}

func extractFrames(
	ctx context.Context,
	paths storage.Paths,
	source, kind string,
	duration float64,
) ([][]byte, error) {
	if kind == "audio" || kind == "font" {
		return nil, nil
	}
	if kind == "image" {
		data, err := os.ReadFile(source)
		return [][]byte{data}, err
	}
	timestamps := []float64{0, duration / 2, max(0, duration-0.1)}
	frames := make([][]byte, 0, len(timestamps))
	for index, timestamp := range timestamps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := filepath.Join(paths.Temporary, fmt.Sprintf("understand-%d-%d.jpg", time.Now().UnixNano(), index))
		_, err := media.RunCommand(ctx, "ffmpeg", "-y", "-ss", fmt.Sprintf("%.3f", timestamp),
			"-i", source, "-frames:v", "1", "-vf", "scale='min(640,iw)':-2", "-q:v", "4", path)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		_ = os.Remove(path)
		if err != nil {
			return nil, err
		}
		frames = append(frames, data)
	}
	return frames, nil
}

func numeric(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	default:
		return 0
	}
}

func kindLabel(kind string) string {
	return map[string]string{"video": "视频", "audio": "音频", "image": "图片", "font": "字体"}[kind]
}

func semanticRole(kind string) string {
	if kind == "audio" {
		return "audio"
	}
	if kind == "image" {
		return "still"
	}
	return "visual"
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
