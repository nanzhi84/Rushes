package media

import (
	"context"
	"errors"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultSceneThreshold uses FFmpeg scdet's native 0-100 score scale.
	DefaultSceneThreshold      = 10.0
	DefaultSceneDownscaleWidth = 640
	sceneAnalysisMethod        = "ffmpeg-scdet-metadata"
	sceneDuplicateEpsilon      = 0.000001
)

// SceneDetectionOptions controls FFmpeg's candidate generation. A zero
// Threshold selects DefaultSceneThreshold. A zero Timeout leaves lifetime
// control entirely to ctx. DownscaleWidth is a maximum analysis width: zero
// selects DefaultSceneDownscaleWidth and a negative value disables scaling.
type SceneDetectionOptions struct {
	Threshold      float64
	Timeout        time.Duration
	DownscaleWidth int
}

// SceneCandidate identifies a possible visual cut without modifying or
// splitting the source file. Score is FFmpeg scdet's 0-100 confidence score.
type SceneCandidate struct {
	PTSTimeSeconds float64 `json:"pts_time_seconds"`
	Score          float64 `json:"score"`
}

type SceneDetection struct {
	Candidates     []SceneCandidate `json:"candidates"`
	Threshold      float64          `json:"threshold"`
	DownscaleWidth int              `json:"downscale_width"`
	AnalysisMethod string           `json:"analysis_method"`
}

// DetectSceneCandidates asks FFmpeg scdet for candidate metadata only. It does
// not cut, transcode, or write media files. The returned candidates are sorted
// by presentation timestamp and duplicate timestamps are collapsed.
func DetectSceneCandidates(
	ctx context.Context,
	source string,
	options SceneDetectionOptions,
) (SceneDetection, error) {
	threshold, downscaleWidth, err := normalizeSceneDetectionOptions(options)
	if err != nil {
		return SceneDetection{}, err
	}
	if strings.TrimSpace(source) == "" {
		return SceneDetection{}, errors.New("镜头检测素材路径不能为空")
	}

	analysisCtx := ctx
	cancel := func() {}
	if options.Timeout > 0 {
		analysisCtx, cancel = context.WithTimeout(ctx, options.Timeout)
	}
	defer cancel()

	thresholdArgument := strconv.FormatFloat(threshold, 'f', -1, 64)
	filter := ""
	if downscaleWidth > 0 {
		filter = "scale=w='min(iw," + strconv.Itoa(downscaleWidth) + ")':h=-2:flags=fast_bilinear,"
	}
	filter += "scdet=threshold=" + thresholdArgument +
		",metadata=mode=select:key=lavfi.scd.time" +
		",metadata=mode=print:file=-:direct=1"
	result, commandErr := RunCommand(
		analysisCtx,
		"ffmpeg",
		"-hide_banner",
		"-nostats",
		"-loglevel", "error",
		"-i", source,
		"-map", "0:v:0",
		"-vf", filter,
		"-an",
		"-sn",
		"-dn",
		"-f", "null",
		"-",
	)
	if commandErr != nil {
		if contextErr := analysisCtx.Err(); contextErr != nil {
			return SceneDetection{}, contextErr
		}
		return SceneDetection{}, commandErr
	}

	return SceneDetection{
		Candidates:     parseSceneCandidates(result.Stdout, threshold),
		Threshold:      threshold,
		DownscaleWidth: downscaleWidth,
		AnalysisMethod: sceneAnalysisMethod,
	}, nil
}

func normalizeSceneDetectionOptions(options SceneDetectionOptions) (float64, int, error) {
	threshold := options.Threshold
	if threshold == 0 {
		threshold = DefaultSceneThreshold
	}
	if math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold < 0 || threshold > 100 {
		return 0, 0, errors.New("镜头检测阈值必须在 0 到 100 之间")
	}
	if options.Timeout < 0 {
		return 0, 0, errors.New("镜头检测超时不能为负数")
	}
	downscaleWidth := options.DownscaleWidth
	if downscaleWidth == 0 {
		downscaleWidth = DefaultSceneDownscaleWidth
	} else if downscaleWidth < 0 {
		downscaleWidth = 0
	}
	return threshold, downscaleWidth, nil
}

const sceneFloatPattern = `[-+]?([0-9]+([.][0-9]*)?|[.][0-9]+)([eE][-+]?[0-9]+)?`

var (
	scenePTSPattern   = regexp.MustCompile(`pts_time\s*[:=]\s*(` + sceneFloatPattern + `)`)
	sceneScorePattern = regexp.MustCompile(`lavfi[.]scd[.]score\s*[:=]\s*(` + sceneFloatPattern + `)`)
	sceneTimePattern  = regexp.MustCompile(`lavfi[.]scd[.]time\s*[:=]\s*(` + sceneFloatPattern + `)`)
)

type pendingSceneCandidate struct {
	ptsTime   float64
	hasPTS    bool
	sceneTime float64
	hasTime   bool
	score     float64
	hasScore  bool
}

func parseSceneCandidates(output []byte, threshold float64) []SceneCandidate {
	candidates := make([]SceneCandidate, 0)
	pending := pendingSceneCandidate{}
	flush := func() {
		if !pending.hasScore || pending.score < threshold || math.IsNaN(pending.score) ||
			math.IsInf(pending.score, 0) {
			pending = pendingSceneCandidate{}
			return
		}
		seconds := pending.sceneTime
		if pending.hasPTS {
			seconds = pending.ptsTime
		} else if !pending.hasTime {
			pending = pendingSceneCandidate{}
			return
		}
		if seconds >= 0 && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) {
			candidates = append(candidates, SceneCandidate{
				PTSTimeSeconds: seconds,
				Score:          pending.score,
			})
		}
		pending = pendingSceneCandidate{}
	}

	for _, line := range strings.Split(string(output), "\n") {
		ptsMatch := scenePTSPattern.FindStringSubmatch(line)
		scoreMatch := sceneScorePattern.FindStringSubmatch(line)
		timeMatch := sceneTimePattern.FindStringSubmatch(line)
		if len(ptsMatch) > 1 {
			if pending.hasPTS || pending.hasTime || pending.hasScore {
				flush()
			}
		} else if len(scoreMatch) > 1 && pending.hasScore {
			// metadata=print starts every canonical block with pts_time. Some
			// FFmpeg builds also emit a compact scdet log containing only a
			// second score/time pair, which likewise starts a new candidate.
			flush()
		}
		if len(ptsMatch) > 1 {
			if value, err := strconv.ParseFloat(ptsMatch[1], 64); err == nil {
				pending.ptsTime = value
				pending.hasPTS = true
			}
		}
		if len(scoreMatch) > 1 {
			if value, err := strconv.ParseFloat(scoreMatch[1], 64); err == nil {
				pending.score = value
				pending.hasScore = true
			}
		}
		if len(timeMatch) > 1 {
			if value, err := strconv.ParseFloat(timeMatch[1], 64); err == nil {
				pending.sceneTime = value
				pending.hasTime = true
			}
		}
	}
	flush()

	sort.SliceStable(candidates, func(left, right int) bool {
		return candidates[left].PTSTimeSeconds < candidates[right].PTSTimeSeconds
	})
	deduplicated := make([]SceneCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		last := len(deduplicated) - 1
		if last >= 0 && math.Abs(candidate.PTSTimeSeconds-deduplicated[last].PTSTimeSeconds) <= sceneDuplicateEpsilon {
			if candidate.Score > deduplicated[last].Score {
				deduplicated[last].Score = candidate.Score
			}
			continue
		}
		deduplicated = append(deduplicated, candidate)
	}
	return deduplicated
}
