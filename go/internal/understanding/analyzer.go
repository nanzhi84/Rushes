package understanding

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

type ProgressFunc func(note string)

type Segment struct {
	StartSec    float64  `json:"start_s"`
	EndSec      float64  `json:"end_s"`
	Description string   `json:"description"`
	Transcript  *string  `json:"transcript"`
	Tags        []string `json:"tags"`
	Quality     string   `json:"quality"`
	Notes       *string  `json:"notes"`
}

type Summary struct {
	AssetID      string    `json:"asset_id"`
	Version      int       `json:"version"`
	Focus        *string   `json:"focus"`
	SemanticRole string    `json:"semantic_role"`
	Overall      string    `json:"overall"`
	Language     *string   `json:"language"`
	Segments     []Segment `json:"segments"`
	GeneratedAt  string    `json:"generated_at"`
	Model        string    `json:"model"`
	Degraded     []string  `json:"degraded,omitempty"`
}

type Analyzer struct {
	vision model.ToolCallingChatModel
}

func NewAnalyzer(vision model.ToolCallingChatModel) *Analyzer {
	return &Analyzer{vision: vision}
}

func (analyzer *Analyzer) Analyze(
	ctx context.Context,
	database *storage.DB,
	asset storage.Asset,
	focus string,
	progress ProgressFunc,
) (Summary, error) {
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
	progress("view_frames：正在抽取代表帧")
	frames, err := extractFrames(ctx, database.Paths, source, kind, duration)
	if err != nil {
		return Summary{}, err
	}
	overall := fmt.Sprintf("%s 素材，时长约 %.2f 秒。", kindLabel(kind), duration)
	modelName := "deterministic-local"
	if analyzer.vision != nil && len(frames) > 0 {
		progress("view_frames：正在调用 VLM 理解画面")
		description, visionErr := analyzer.describeFrames(ctx, frames, focus)
		if visionErr != nil {
			return Summary{}, visionErr
		}
		if description != "" {
			overall = description
		}
		modelName = "qwen-vlm"
	}
	progress("transcribe：一期未配置 ASR，保留降级提示")
	note := "音频转写不可用；摘要仅基于画面与媒体元数据。"
	if kind == "image" || kind == "font" {
		note = "静态素材无需音频转写。"
	}
	progress("emit_summary：正在生成结构化摘要")
	return Summary{
		AssetID: asset.ID, Version: 1, Focus: stringPointer(focus),
		SemanticRole: semanticRole(kind), Overall: overall,
		Segments: []Segment{{
			StartSec: 0, EndSec: duration, Description: overall,
			Transcript: nil, Tags: []string{kind}, Quality: "usable", Notes: &note,
		}},
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), Model: modelName,
		Degraded: []string{"transcribe_unavailable"},
	}, nil
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
