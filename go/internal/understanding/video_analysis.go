package understanding

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

const (
	understandingTimelineFPS = 30
	frameExtractMaxWidth     = 640
	frameExtractJPEGQuality  = 4
	// scdet only proposes candidates. A lower-than-default threshold preserves
	// recall; the VLM sees frames on both sides and rejects flashes/motion.
	understandingSceneThreshold = 6.0
	maxBoundaryCandidates       = 24
	maxBoundaryCandidatesPerVLM = 8
	maxSegmentSpansPerVLM       = 8
	minimumBoundaryDistanceSec  = 0.12
)

type videoAnalysisResult struct {
	Overall        string
	SemanticRole   string
	Segments       []Segment
	Model          string
	AnalysisMethod string
	CandidateCuts  int
	VerifiedCuts   int
	Degraded       []string
}

type videoBoundary struct {
	TimeSec  float64
	Score    float64
	Verified bool
}

type videoSpan struct {
	ID               string
	StartSec         float64
	EndSec           float64
	BoundaryKind     string
	BoundaryScore    *float64
	BoundaryVerified bool
}

type sampledSegmentFrame struct {
	SegmentID string
	JPEG      []byte
	Label     string
	Timestamp float64
}

type boundaryVerificationPayload struct {
	Boundaries []struct {
		ID         string  `json:"id"`
		Kind       string  `json:"kind"`
		Accept     bool    `json:"accept"`
		Confidence float64 `json:"confidence"`
	} `json:"boundaries"`
}

type segmentDescriptionPayload struct {
	Overall      string `json:"overall"`
	SemanticRole string `json:"semantic_role"`
	Segments     []struct {
		ID          string   `json:"id"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
		Quality     string   `json:"quality"`
		Subjects    []string `json:"subjects"`
		Actions     []string `json:"actions"`
		Setting     []string `json:"setting"`
		ShotScale   string   `json:"shot_scale"`
		Composition string   `json:"composition"`
		Lighting    []string `json:"lighting"`
		Mood        []string `json:"mood"`
		EditHints   []string `json:"edit_hints"`
	} `json:"segments"`
}

func (analyzer *Analyzer) analyzeVideo(
	ctx context.Context,
	paths storage.Paths,
	source string,
	durationSec float64,
	options AnalyzeOptions,
	progress ProgressFunc,
) (videoAnalysisResult, error) {
	result := videoAnalysisResult{
		Overall:        fmt.Sprintf("视频素材，时长约 %.2f 秒。", durationSec),
		Model:          "deterministic-local",
		AnalysisMethod: "ffmpeg-scdet+analysis-windows",
	}
	progress("scene_detect：正在扫描候选切镜")
	detection, detectErr := media.DetectSceneCandidates(ctx, source, media.SceneDetectionOptions{
		Threshold: understandingSceneThreshold,
		Timeout:   sceneDetectionTimeout(durationSec),
	})
	if detectErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return videoAnalysisResult{}, ctxErr
		}
		result.Degraded = append(result.Degraded, "scene_detection_unavailable")
	}
	result.CandidateCuts = len(detection.Candidates)
	candidates, truncated := selectBoundaryCandidates(detection.Candidates, durationSec, maxBoundaryCandidates)
	if truncated {
		result.Degraded = append(result.Degraded, "scene_candidates_truncated")
	}

	var boundaries []videoBoundary
	if analyzer.vision != nil && len(candidates) > 0 {
		progress("scene_verify：正在让 VLM 区分真切镜、闪光与运镜")
		verified, verifyErr := analyzer.verifySceneCandidates(
			ctx, paths, source, durationSec, candidates, options.Focus,
		)
		if verifyErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return videoAnalysisResult{}, ctxErr
			}
			result.Degraded = append(result.Degraded, "scene_candidate_verification_unavailable")
			boundaries = unverifiedBoundaries(candidates)
		} else {
			boundaries = verified
			for _, boundary := range verified {
				if boundary.Verified {
					result.VerifiedCuts++
				}
			}
			result.AnalysisMethod += "+qwen-vlm-boundary-verification"
		}
	} else {
		boundaries = unverifiedBoundaries(candidates)
		if len(candidates) > 0 {
			result.Degraded = append(result.Degraded, "scene_candidates_unverified")
		}
	}

	spans := buildVideoSpans(durationSec, boundaries, options)
	result.Segments = segmentsFromSpans(spans)
	progress("view_frames：正在按切镜与长镜头窗口抽取代表帧")
	samples, extractDegraded, extractErr := extractSegmentFrames(ctx, paths, source, spans, options)
	if extractErr != nil {
		return videoAnalysisResult{}, extractErr
	}
	if extractDegraded {
		result.Degraded = append(result.Degraded, "representative_frame_extract_partial")
	}
	applyFrameQualityMetrics(&result, samples)
	if analyzer.vision == nil || len(samples) == 0 {
		result.Degraded = append(result.Degraded, "visual_understanding_unavailable")
		return result, nil
	}

	progress("view_frames：正在调用 VLM 生成逐镜头摘要")
	description, describeErr := analyzer.describeSegmentFrames(ctx, samples, options.Focus)
	if describeErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return videoAnalysisResult{}, ctxErr
		}
		// Visual providers are optional enrichment. Keep deterministic temporal
		// evidence so one transient provider failure cannot fail the whole tool.
		result.Degraded = append(result.Degraded, "visual_summary_unavailable")
		return result, nil
	}
	applySegmentDescriptions(&result, description)
	result.Model = "qwen-vlm"
	return result, nil
}

func sceneDetectionTimeout(durationSec float64) time.Duration {
	seconds := max(20, min(120, int(math.Ceil(durationSec*2))))
	return time.Duration(seconds) * time.Second
}

func selectBoundaryCandidates(
	candidates []media.SceneCandidate,
	durationSec float64,
	limit int,
) ([]media.SceneCandidate, bool) {
	valid := make([]media.SceneCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.PTSTimeSeconds <= minimumBoundaryDistanceSec ||
			candidate.PTSTimeSeconds >= durationSec-minimumBoundaryDistanceSec {
			continue
		}
		valid = append(valid, candidate)
	}
	if limit <= 0 || len(valid) <= limit {
		return valid, false
	}
	sort.SliceStable(valid, func(left, right int) bool {
		return valid[left].Score > valid[right].Score
	})
	valid = valid[:limit]
	sort.SliceStable(valid, func(left, right int) bool {
		return valid[left].PTSTimeSeconds < valid[right].PTSTimeSeconds
	})
	return valid, true
}

func unverifiedBoundaries(candidates []media.SceneCandidate) []videoBoundary {
	boundaries := make([]videoBoundary, 0, len(candidates))
	for _, candidate := range candidates {
		boundaries = append(boundaries, videoBoundary{
			TimeSec: candidate.PTSTimeSeconds, Score: candidate.Score,
		})
	}
	return boundaries
}

func (analyzer *Analyzer) verifySceneCandidates(
	ctx context.Context,
	paths storage.Paths,
	source string,
	durationSec float64,
	candidates []media.SceneCandidate,
	focus string,
) ([]videoBoundary, error) {
	verified := make([]videoBoundary, 0, len(candidates))
	for start := 0; start < len(candidates); start += maxBoundaryCandidatesPerVLM {
		end := min(len(candidates), start+maxBoundaryCandidatesPerVLM)
		batch, err := analyzer.verifySceneCandidateBatch(
			ctx, paths, source, durationSec, candidates[start:end], focus, start,
		)
		if err != nil {
			return nil, err
		}
		verified = append(verified, batch...)
	}
	return verified, nil
}

func (analyzer *Analyzer) verifySceneCandidateBatch(
	ctx context.Context,
	paths storage.Paths,
	source string,
	durationSec float64,
	candidates []media.SceneCandidate,
	focus string,
	idOffset int,
) ([]videoBoundary, error) {
	prompt := `你在复核 FFmpeg scdet 给出的候选切镜。每个候选依次提供切点前、后两张图。
只有相机镜头/场景真正发生切换或明确渐变转场时 accept=true；火光、闪光、遮挡、曝光变化、快速运镜或同一连续动作必须 accept=false。
严格只返回 JSON：{"boundaries":[{"id":"c000","kind":"cut|fade|flash|motion|same_shot|uncertain","accept":true,"confidence":0.0}]}。每个 id 必须出现一次。`
	if strings.TrimSpace(focus) != "" {
		prompt += "\n当前剪辑重点：" + focus
	}
	parts := []schema.MessageInputPart{{Type: schema.ChatMessagePartTypeText, Text: prompt}}
	ids := make([]string, 0, len(candidates))
	for index, candidate := range candidates {
		id := fmt.Sprintf("c%03d", idOffset+index)
		ids = append(ids, id)
		delta := min(0.10, max(0.04, durationSec/1000))
		before, err := extractFrameAt(ctx, paths, source, max(0, candidate.PTSTimeSeconds-delta))
		if err != nil {
			return nil, err
		}
		after, err := extractFrameAt(ctx, paths, source, min(durationSec-0.001, candidate.PTSTimeSeconds+delta))
		if err != nil {
			return nil, err
		}
		parts = append(parts,
			schema.MessageInputPart{Type: schema.ChatMessagePartTypeText, Text: fmt.Sprintf(
				"候选 %s，PTS %.6f 秒，scdet score %.3f；下两图依次为 before / after。",
				id, candidate.PTSTimeSeconds, candidate.Score,
			)},
			jpegMessagePart(before),
			jpegMessagePart(after),
		)
	}
	response, err := analyzer.vision.Generate(ctx, []*schema.Message{{
		Role: schema.User, UserInputMultiContent: parts,
	}})
	if err != nil {
		return nil, err
	}
	payload := boundaryVerificationPayload{}
	if err := decodeJSONObject(response.Content, &payload); err != nil {
		return nil, fmt.Errorf("切镜复核返回无效 JSON: %w", err)
	}
	decisions := map[string]struct {
		kind       string
		accept     bool
		confidence float64
	}{}
	for _, boundary := range payload.Boundaries {
		decisions[boundary.ID] = struct {
			kind       string
			accept     bool
			confidence float64
		}{
			kind:   strings.ToLower(strings.TrimSpace(boundary.Kind)),
			accept: boundary.Accept, confidence: boundary.Confidence,
		}
	}
	verified := make([]videoBoundary, 0, len(candidates))
	for index, candidate := range candidates {
		decision, exists := decisions[ids[index]]
		if !exists {
			// Missing rows are treated as unverified candidates, preserving recall.
			verified = append(verified, videoBoundary{TimeSec: candidate.PTSTimeSeconds, Score: candidate.Score})
			continue
		}
		acceptedKind := decision.kind == "cut" || decision.kind == "fade"
		if !decision.accept || !acceptedKind || decision.confidence < 0.5 {
			continue
		}
		verified = append(verified, videoBoundary{
			TimeSec: candidate.PTSTimeSeconds, Score: candidate.Score, Verified: true,
		})
	}
	return verified, nil
}

func buildVideoSpans(durationSec float64, boundaries []videoBoundary, options AnalyzeOptions) []videoSpan {
	maxSegments, maxWindowSec := videoAnalysisBudget(options)
	sort.SliceStable(boundaries, func(left, right int) bool {
		return boundaries[left].TimeSec < boundaries[right].TimeSec
	})
	filtered := make([]videoBoundary, 0, len(boundaries))
	for _, boundary := range boundaries {
		if boundary.TimeSec <= minimumBoundaryDistanceSec ||
			boundary.TimeSec >= durationSec-minimumBoundaryDistanceSec {
			continue
		}
		if len(filtered) > 0 && boundary.TimeSec-filtered[len(filtered)-1].TimeSec < minimumBoundaryDistanceSec {
			if boundary.Score > filtered[len(filtered)-1].Score {
				filtered[len(filtered)-1] = boundary
			}
			continue
		}
		filtered = append(filtered, boundary)
	}
	if len(filtered)+1 > maxSegments {
		sort.SliceStable(filtered, func(left, right int) bool {
			return filtered[left].Score > filtered[right].Score
		})
		filtered = filtered[:maxSegments-1]
		sort.SliceStable(filtered, func(left, right int) bool {
			return filtered[left].TimeSec < filtered[right].TimeSec
		})
	}
	spans := make([]videoSpan, 0, maxSegments)
	if len(filtered) == 0 {
		spans = []videoSpan{{StartSec: 0, EndSec: durationSec, BoundaryKind: "video_start"}}
	} else {
		spans = append(spans, videoSpan{StartSec: 0, EndSec: filtered[0].TimeSec, BoundaryKind: "video_start"})
		for index, boundary := range filtered {
			end := durationSec
			if index+1 < len(filtered) {
				end = filtered[index+1].TimeSec
			}
			score := boundary.Score
			spans = append(spans, videoSpan{
				StartSec: boundary.TimeSec, EndSec: end, BoundaryKind: "visual_cut",
				BoundaryScore: &score, BoundaryVerified: boundary.Verified,
			})
		}
	}
	for len(spans) < maxSegments {
		longestIndex := -1
		longestDuration := maxWindowSec
		for index, span := range spans {
			if span.EndSec-span.StartSec > longestDuration {
				longestIndex = index
				longestDuration = span.EndSec - span.StartSec
			}
		}
		if longestIndex < 0 {
			break
		}
		original := spans[longestIndex]
		midpoint := original.StartSec + (original.EndSec-original.StartSec)/2
		left, right := original, original
		left.EndSec = midpoint
		right.StartSec = midpoint
		right.BoundaryKind = "analysis_window"
		right.BoundaryScore = nil
		right.BoundaryVerified = false
		spans = append(spans[:longestIndex], append([]videoSpan{left, right}, spans[longestIndex+1:]...)...)
	}
	for index := range spans {
		spans[index].ID = fmt.Sprintf("s%03d", index)
	}
	return spans
}

func videoAnalysisBudget(options AnalyzeOptions) (int, float64) {
	if options.MaxStepsPerAsset > 0 {
		return max(1, min(24, options.MaxStepsPerAsset)), 6
	}
	if strings.EqualFold(strings.TrimSpace(options.Depth), "deep") {
		return 16, 6
	}
	return 8, 12
}

func segmentsFromSpans(spans []videoSpan) []Segment {
	segments := make([]Segment, 0, len(spans))
	for _, span := range spans {
		startFrame := max(0, int(math.Round(span.StartSec*understandingTimelineFPS)))
		endFrame := max(startFrame+1, int(math.Round(span.EndSec*understandingTimelineFPS)))
		description := fmt.Sprintf("待理解视频片段，源区间 %.2f–%.2f 秒。", span.StartSec, span.EndSec)
		segments = append(segments, Segment{
			StartSec: span.StartSec, EndSec: span.EndSec,
			SourceStartFrame: startFrame, SourceEndFrame: endFrame,
			Description: description, Tags: []string{"video"}, Quality: "usable",
			BoundaryKind: span.BoundaryKind, BoundaryScore: span.BoundaryScore,
			BoundaryVerified: span.BoundaryVerified,
		})
	}
	return segments
}

func extractSegmentFrames(
	ctx context.Context,
	paths storage.Paths,
	source string,
	spans []videoSpan,
	options AnalyzeOptions,
) ([]sampledSegmentFrame, bool, error) {
	samples := make([]sampledSegmentFrame, 0, len(spans)*3)
	degraded := false
	for _, span := range spans {
		if err := ctx.Err(); err != nil {
			return nil, degraded, err
		}
		timestamps := segmentFrameTimestamps(span, options.Depth)
		for index, timestamp := range timestamps {
			jpegBytes, err := extractFrameAt(ctx, paths, source, timestamp)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil, degraded, err
				}
				degraded = true
				continue
			}
			samples = append(samples, sampledSegmentFrame{
				SegmentID: span.ID, JPEG: jpegBytes, Timestamp: timestamp,
				Label: segmentFrameLabel(span, timestamps, index, options.Depth),
			})
		}
	}
	return samples, degraded, nil
}

func segmentFrameLabel(span videoSpan, timestamps []float64, index int, depth string) string {
	position := "中帧"
	if strings.EqualFold(strings.TrimSpace(depth), "deep") {
		switch {
		case len(timestamps) == 2 && index == 0:
			position = "首帧"
		case len(timestamps) == 2 && index == 1:
			position = "尾帧"
		case len(timestamps) >= 3:
			position = []string{"首帧", "中帧", "尾帧"}[min(index, 2)]
		}
	}
	timestamp := timestamps[index]
	return fmt.Sprintf("%s %.2f–%.2f 秒，%s %.3f 秒", span.ID, span.StartSec, span.EndSec, position, timestamp)
}

func segmentFrameTimestamps(span videoSpan, depth string) []float64 {
	duration := max(0.0, span.EndSec-span.StartSec)
	middle := span.StartSec + duration/2
	if !strings.EqualFold(strings.TrimSpace(depth), "deep") || duration <= 0.20 {
		return []float64{middle}
	}
	first := min(span.EndSec-0.001, span.StartSec+0.10)
	last := max(span.StartSec, span.EndSec-0.10)
	values := []float64{first, middle, last}
	if duration <= 0.30 {
		values = []float64{first, last}
	}
	result := make([]float64, 0, len(values))
	for _, value := range values {
		value = min(max(span.StartSec, value), max(span.StartSec, span.EndSec-0.001))
		duplicate := false
		for _, existing := range result {
			duplicate = duplicate || math.Abs(existing-value) < 0.0005
		}
		if !duplicate {
			result = append(result, value)
		}
	}
	return result
}

func applyFrameQualityMetrics(result *videoAnalysisResult, samples []sampledSegmentFrame) {
	type aggregate struct {
		overexposed float64
		sharpness   float64
		count       int
	}
	bySegment := map[string]aggregate{}
	for _, sample := range samples {
		overexposed, sharpness, err := frameQualityMetrics(sample.JPEG)
		if err != nil {
			continue
		}
		value := bySegment[sample.SegmentID]
		value.overexposed += overexposed
		value.sharpness += sharpness
		value.count++
		bySegment[sample.SegmentID] = value
	}
	for index := range result.Segments {
		value := bySegment[fmt.Sprintf("s%03d", index)]
		if value.count == 0 {
			continue
		}
		overexposed := math.Round(value.overexposed/float64(value.count)*10000) / 10000
		sharpness := math.Round(value.sharpness/float64(value.count)*100) / 100
		result.Segments[index].OverexposedRatio = &overexposed
		result.Segments[index].SharpnessScore = &sharpness
	}
}

func frameQualityMetrics(jpegBytes []byte) (float64, float64, error) {
	decoded, err := jpeg.Decode(bytes.NewReader(jpegBytes))
	if err != nil {
		return 0, 0, err
	}
	bounds := decoded.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width == 0 || height == 0 {
		return 0, 0, errors.New("空 JPEG 帧")
	}
	gray := make([]float64, width*height)
	overexposed := 0
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			r, g, b, _ := decoded.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			luma := 0.2126*float64(r>>8) + 0.7152*float64(g>>8) + 0.0722*float64(b>>8)
			gray[y*width+x] = luma
			if luma >= 250 {
				overexposed++
			}
		}
	}
	laplacians := make([]float64, 0, max(0, (width-2)*(height-2)))
	for y := 1; y < height-1; y++ {
		for x := 1; x < width-1; x++ {
			index := y*width + x
			laplacians = append(laplacians, 4*gray[index]-gray[index-1]-gray[index+1]-gray[index-width]-gray[index+width])
		}
	}
	mean := 0.0
	for _, value := range laplacians {
		mean += value
	}
	if len(laplacians) > 0 {
		mean /= float64(len(laplacians))
	}
	variance := 0.0
	for _, value := range laplacians {
		variance += (value - mean) * (value - mean)
	}
	if len(laplacians) > 0 {
		variance /= float64(len(laplacians))
	}
	return float64(overexposed) / float64(width*height), variance, nil
}

func extractFrameAt(ctx context.Context, paths storage.Paths, source string, timestampSec float64) ([]byte, error) {
	file, err := os.CreateTemp(paths.Temporary, "understand-frame-*.jpg")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	if closeErr := file.Close(); closeErr != nil {
		_ = os.Remove(path)
		return nil, closeErr
	}
	defer func() { _ = os.Remove(path) }()
	_, err = media.RunCommand(
		ctx, "ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-ss", fmt.Sprintf("%.6f", max(0, timestampSec)), "-i", source,
		"-frames:v", "1", "-vf", fmt.Sprintf("scale='min(%d,iw)':-2", frameExtractMaxWidth),
		"-q:v", fmt.Sprintf("%d", frameExtractJPEGQuality), path,
	)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func jpegMessagePart(jpeg []byte) schema.MessageInputPart {
	encoded := base64.StdEncoding.EncodeToString(jpeg)
	return schema.MessageInputPart{
		Type: schema.ChatMessagePartTypeImageURL,
		Image: &schema.MessageInputImage{
			MessagePartCommon: schema.MessagePartCommon{Base64Data: &encoded, MIMEType: "image/jpeg"},
			Detail:            schema.ImageURLDetailHigh,
		},
	}
}

func (analyzer *Analyzer) describeSegmentFrames(
	ctx context.Context,
	samples []sampledSegmentFrame,
	focus string,
) (string, error) {
	if len(samples) == 0 {
		return "", nil
	}
	responses := []string{}
	for start := 0; start < len(samples); {
		spanCount := 0
		end := start
		lastID := ""
		for end < len(samples) {
			if samples[end].SegmentID != lastID {
				if spanCount == maxSegmentSpansPerVLM {
					break
				}
				spanCount++
				lastID = samples[end].SegmentID
			}
			end++
		}
		response, err := analyzer.describeSegmentFrameBatch(ctx, samples[start:end], focus)
		if err != nil {
			return "", err
		}
		if err := validateSegmentDescriptionResponse(response, samples[start:end]); err != nil {
			return "", err
		}
		responses = append(responses, response)
		start = end
	}
	return mergeSegmentDescriptionResponses(responses), nil
}

func (analyzer *Analyzer) describeSegmentFrameBatch(
	ctx context.Context,
	samples []sampledSegmentFrame,
	focus string,
) (string, error) {
	prompt := `你正在为视频剪辑 Agent 建立可检索的逐镜头语义索引。后续每张图都附带 segment id 和确切源时间。
只描述画面可见事实，但要尽量具体：主体身份或外观、场景、正在发生的动作、景别、构图、光线与色调、情绪氛围，以及适合怎样剪辑。description 必须是一句信息密集的中文检索文本，避免“画面很好看”之类空泛评价。一个 segment 有首/中/尾多帧时，可依据帧间构图变化描述段内动作趋势、推近、拉远或横移方向；只有单帧时不要猜测运动。edit_hints 写可执行用途，例如“适合高潮强拍切入”“适合作为环境建立镜头”。
同时判断整段素材在口播工作流中的客观角色：人物直接面对镜头讲解、采访或连续表达为 a_roll；产品展示、操作演示、环境、细节、对比等用于覆盖讲述内容的画面为 b_roll。只依据可见证据，无法判断时返回空字符串。
严格只返回 JSON：{"overall":"整体内容、视觉风格与可剪用途摘要","semantic_role":"a_roll|b_roll|","segments":[{"id":"s000","description":"夜晚海滩上红衣女性举起火把，橙色火光照亮人物，中景居中构图，适合高潮切入","tags":["人物","海滩","火光","夜景"],"quality":"usable|soft|dark|blurred","subjects":["红衣女性"],"actions":["举起火把"],"setting":["夜晚海滩"],"shot_scale":"中景","composition":"主体居中","lighting":["橙色火光","低照度"],"mood":["神秘","高能"],"edit_hints":["高潮强拍切入"]}]}。每个 id 必须出现一次。`
	if strings.TrimSpace(focus) != "" {
		prompt += "\n剪辑重点：" + focus
	}
	parts := []schema.MessageInputPart{{Type: schema.ChatMessagePartTypeText, Text: prompt}}
	for _, sample := range samples {
		parts = append(parts,
			schema.MessageInputPart{Type: schema.ChatMessagePartTypeText, Text: "segment " + sample.Label},
			jpegMessagePart(sample.JPEG),
		)
	}
	response, err := analyzer.vision.Generate(ctx, []*schema.Message{{
		Role: schema.User, UserInputMultiContent: parts,
	}})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(response.Content), nil
}

func validateSegmentDescriptionResponse(response string, samples []sampledSegmentFrame) error {
	payload := segmentDescriptionPayload{}
	if err := decodeJSONObject(response, &payload); err != nil {
		return fmt.Errorf("逐镜头视觉摘要不是有效 JSON: %w", err)
	}
	expected := map[string]struct{}{}
	for _, sample := range samples {
		expected[sample.SegmentID] = struct{}{}
	}
	seen := map[string]struct{}{}
	for _, segment := range payload.Segments {
		id := strings.TrimSpace(segment.ID)
		if _, ok := expected[id]; !ok {
			return fmt.Errorf("逐镜头视觉摘要包含非预期 segment id %q", id)
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("逐镜头视觉摘要重复 segment id %q", id)
		}
		seen[id] = struct{}{}
	}
	missing := make([]string, 0)
	for id := range expected {
		if _, ok := seen[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("逐镜头视觉摘要缺少 segment id: %s", strings.Join(missing, "、"))
	}
	return nil
}

func mergeSegmentDescriptionResponses(responses []string) string {
	if len(responses) == 1 {
		return strings.TrimSpace(responses[0])
	}
	merged := segmentDescriptionPayload{}
	overallSeen := map[string]struct{}{}
	roleCounts := map[string]int{}
	for _, response := range responses {
		payload := segmentDescriptionPayload{}
		_ = decodeJSONObject(response, &payload) // 每批已在合并前完成严格完整性校验。
		overall := strings.TrimSpace(payload.Overall)
		if overall != "" {
			if _, exists := overallSeen[overall]; !exists {
				overallSeen[overall] = struct{}{}
				if merged.Overall != "" {
					merged.Overall += "；"
				}
				merged.Overall += overall
			}
		}
		role := normalizeVisualRole(payload.SemanticRole)
		if role != "" {
			// Each response has already passed segment completeness validation, so
			// its segment count is the number of distinct shots represented by the
			// batch. Weight the material-level role by shots rather than requests.
			roleCounts[role] += len(payload.Segments)
		}
		merged.Segments = append(merged.Segments, payload.Segments...)
	}
	if roleCounts["a_roll"] >= roleCounts["b_roll"] && roleCounts["a_roll"] > 0 {
		merged.SemanticRole = "a_roll"
	} else if roleCounts["b_roll"] > 0 {
		merged.SemanticRole = "b_roll"
	}
	encoded, _ := json.Marshal(merged)
	return string(encoded)
}

func applySegmentDescriptions(result *videoAnalysisResult, raw string) {
	payload := segmentDescriptionPayload{}
	if decodeJSONObject(raw, &payload) != nil {
		// Older/less strict providers may still return a useful paragraph. Keep
		// it as the overall summary without inventing per-segment semantics.
		if strings.TrimSpace(raw) != "" {
			result.Overall = strings.TrimSpace(raw)
			if len(result.Segments) == 1 {
				result.Segments[0].Description = strings.TrimSpace(raw)
			}
		}
		return
	}
	if strings.TrimSpace(payload.Overall) != "" {
		result.Overall = strings.TrimSpace(payload.Overall)
	}
	result.SemanticRole = normalizeVisualRole(payload.SemanticRole)
	byID := make(map[string]struct {
		description string
		tags        []string
		quality     string
		subjects    []string
		actions     []string
		setting     []string
		shotScale   string
		composition string
		lighting    []string
		mood        []string
		editHints   []string
	}, len(payload.Segments))
	for _, segment := range payload.Segments {
		byID[segment.ID] = struct {
			description string
			tags        []string
			quality     string
			subjects    []string
			actions     []string
			setting     []string
			shotScale   string
			composition string
			lighting    []string
			mood        []string
			editHints   []string
		}{
			description: strings.TrimSpace(segment.Description),
			tags:        uniqueNonEmptyStrings(append([]string{"video"}, segment.Tags...)),
			quality:     normalizeVisualQuality(segment.Quality),
			subjects:    uniqueNonEmptyStrings(segment.Subjects),
			actions:     uniqueNonEmptyStrings(segment.Actions),
			setting:     uniqueNonEmptyStrings(segment.Setting),
			shotScale:   strings.TrimSpace(segment.ShotScale),
			composition: strings.TrimSpace(segment.Composition),
			lighting:    uniqueNonEmptyStrings(segment.Lighting),
			mood:        uniqueNonEmptyStrings(segment.Mood),
			editHints:   uniqueNonEmptyStrings(segment.EditHints),
		}
	}
	for index := range result.Segments {
		id := fmt.Sprintf("s%03d", index)
		description, exists := byID[id]
		if !exists {
			continue
		}
		if description.description != "" {
			result.Segments[index].Description = description.description
		}
		if len(description.tags) > 0 {
			result.Segments[index].Tags = description.tags
		}
		result.Segments[index].Quality = description.quality
		result.Segments[index].Subjects = description.subjects
		result.Segments[index].Actions = description.actions
		result.Segments[index].Setting = description.setting
		result.Segments[index].ShotScale = description.shotScale
		result.Segments[index].Composition = description.composition
		result.Segments[index].Lighting = description.lighting
		result.Segments[index].Mood = description.mood
		result.Segments[index].EditHints = description.editHints
	}
}

func decodeJSONObject(raw string, target any) error {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	trimmed = strings.TrimSpace(trimmed)
	start, end := strings.Index(trimmed, "{"), strings.LastIndex(trimmed, "}")
	if start < 0 || end < start {
		return errors.New("未找到 JSON 对象")
	}
	return json.Unmarshal([]byte(trimmed[start:end+1]), target)
}

func normalizeVisualQuality(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "soft", "dark", "blurred":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "usable"
	}
}

func uniqueNonEmptyStrings(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
