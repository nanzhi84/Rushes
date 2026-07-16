package tools

import (
	"context"
	"errors"
)

type contextKey string

const (
	draftIDKey                contextKey = "rushes_draft_id"
	reporterKey               contextKey = "rushes_tool_reporter"
	timelineMutationOriginKey contextKey = "rushes_timeline_mutation_origin"
	toolCallIDKey             contextKey = "rushes_tool_call_id"
)

type Executor interface {
	ExecuteTool(context.Context, string, any) (any, error)
}

type Reporter func(name, phase string, input, output any, err error)

func WithDraftID(ctx context.Context, draftID string) context.Context {
	return context.WithValue(ctx, draftIDKey, draftID)
}

func DraftID(ctx context.Context) (string, error) {
	value, _ := ctx.Value(draftIDKey).(string)
	if value == "" {
		return "", errors.New("工具执行缺少 active draft")
	}
	return value, nil
}

// WithTimelineMutationOrigin 标记来自编辑器 REST 会话的人工时间线提交。
// 未设置时，时间线工具按 Agent 调用处理。
func WithTimelineMutationOrigin(ctx context.Context, origin string) context.Context {
	return context.WithValue(ctx, timelineMutationOriginKey, origin)
}

func TimelineMutationOrigin(ctx context.Context) string {
	value, _ := ctx.Value(timelineMutationOriginKey).(string)
	return value
}

// WithToolCallID carries the model tool-call identity through middleware into
// the reducer-backed executor. Direct REST and test calls intentionally leave it empty.
func WithToolCallID(ctx context.Context, toolCallID string) context.Context {
	return context.WithValue(ctx, toolCallIDKey, toolCallID)
}

func ToolCallID(ctx context.Context) string {
	value, _ := ctx.Value(toolCallIDKey).(string)
	return value
}

func WithReporter(ctx context.Context, reporter Reporter) context.Context {
	return context.WithValue(ctx, reporterKey, reporter)
}

// ReporterFromContext 让工具编排层在不打破实时 started 事件的前提下，
// 合并同一次工具调用的内部重试。普通工具实现仍只需要使用 WithReporter。
func ReporterFromContext(ctx context.Context) (Reporter, bool) {
	reporter, ok := ctx.Value(reporterKey).(Reporter)
	return reporter, ok && reporter != nil
}

type ToolResult struct {
	Status      string         `json:"status"`
	Observation string         `json:"observation"`
	Data        map[string]any `json:"data,omitempty"`
}

type AssetImportInput struct {
	Path        string `json:"path" jsonschema:"required" jsonschema_description:"已由用户在文件选择器确认的本地路径"`
	StorageMode string `json:"storage_mode,omitempty" jsonschema_description:"reference 或 copy"`
	Kind        string `json:"kind,omitempty" jsonschema_description:"video audio image font"`
}

type AssetListInput struct {
	Kind       string `json:"kind,omitempty" jsonschema_description:"可选素材类型筛选：video、audio、image 或 font"`
	OnlyUsable *bool  `json:"only_usable,omitempty" jsonschema_description:"是否只返回当前可用于剪辑的素材；默认 false，设为 true 可排除导入失败或不可读素材"`
	Limit      int    `json:"limit,omitempty" jsonschema_description:"单页返回数量，默认 50，上限 200"`
	After      string `json:"after,omitempty" jsonschema_description:"上一页 next_after 返回的游标；首次读取时省略"`
}

type AssetManifest struct {
	AssetID             string `json:"asset_id" jsonschema_description:"当前草稿中的稳定素材 ID；调用其他素材或时间线工具时原样传递"`
	Filename            string `json:"filename" jsonschema_description:"导入素材的原始文件名，仅用于识别素材，不是可读取的本地路径"`
	Kind                string `json:"kind" jsonschema_description:"素材类型：video、audio、image 或 font；video/image 可作为主视觉，audio 用于音频轨"`
	RelDir              string `json:"rel_dir,omitempty" jsonschema_description:"导入时保留的相对素材目录，可作为 A-roll/B-roll 等用户组织信息"`
	SuggestedRole       string `json:"suggested_role,omitempty" jsonschema_description:"音频的建议轨道角色：bgm 或 sfx"`
	SuggestedVisualRole string `json:"suggested_visual_role,omitempty" jsonschema_description:"视频的可解释初始角色：a_roll 或 b_roll；优先来自用户目录和已持久化素材理解"`
	DurationFrames      int    `json:"duration_frames,omitempty" jsonschema_description:"按 timeline_fps 标尺换算的素材总帧数；选择源区间时不得超过该范围"`
	TimelineFPS         int    `json:"timeline_fps" jsonschema_description:"duration_frames 与所有整数帧坐标使用的每秒帧数标尺"`
	Usable              bool   `json:"usable" jsonschema_description:"素材当前是否可被工具读取和用于剪辑；false 时不要选入时间线"`
	IngestStatus        string `json:"ingest_status" jsonschema_description:"素材导入与代理准备状态；ready 表示导入处理已经完成"`
	UnderstandingStatus string `json:"understanding_status" jsonschema_description:"素材理解状态；ready 表示已有可复用的持久化理解结果"`
}

type AssetListResult struct {
	DraftID   string          `json:"draft_id"`
	Assets    []AssetManifest `json:"assets"`
	Total     int             `json:"total"`
	NextAfter string          `json:"next_after,omitempty"`
	UsageNote string          `json:"usage_note,omitempty"`
}

type UnderstandInput struct {
	AssetIDs         []string `json:"asset_ids" jsonschema:"required" jsonschema_description:"asset.list_assets 返回的一个或多个素材 ID；不要传文件名或本地路径"`
	Depth            string   `json:"depth,omitempty" jsonschema_description:"scan 做低成本广度扫描，deep 做逐镜头深度理解；默认 scan，多素材或 deep 可能异步排队并在完成后自动续跑"`
	Focus            string   `json:"focus,omitempty" jsonschema_description:"可选创作关注点，例如产品特写、人物动作或可用于高潮的镜头；会进入视觉分析提示与缓存键"`
	MaxStepsPerAsset int      `json:"max_steps_per_asset,omitempty" jsonschema_description:"每个素材的最大分析步骤数；0 使用服务端默认值，数值越大成本和延迟越高"`
	ForceRefresh     bool     `json:"force_refresh,omitempty" jsonschema_description:"仅当用户明确要求重新分析时设为 true；默认复用相同素材与参数的持久化结果"`
	RefreshNonce     string   `json:"refresh_nonce,omitempty" jsonschema_description:"仅当用户在旧强制任务终态后明确要求再次重跑完全相同的分析时提供新的短标识；同一标识重复调用仍幂等复用同一 job"`
}

// MaterialEvidence 是模型可直接用于选择源素材区间的紧凑时间证据。
// 秒字段兼容既有 understanding summary；整数帧字段可直接传给时间线工具。
type MaterialEvidence struct {
	StartSec         float64  `json:"start_s"`
	EndSec           float64  `json:"end_s"`
	SourceStartFrame int      `json:"source_start_frame"`
	SourceEndFrame   int      `json:"source_end_frame"`
	Description      string   `json:"description,omitempty"`
	Transcript       string   `json:"transcript,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	Quality          string   `json:"quality,omitempty"`
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

// MaterialUnderstandingSummary 刻意省略模型名、生成时间和历史版本等编排元数据，
// 防止工具结果与后续草稿上下文被无关信息撑大。
type MaterialUnderstandingSummary struct {
	AssetID           string             `json:"asset_id"`
	Filename          string             `json:"filename,omitempty"`
	Kind              string             `json:"kind,omitempty"`
	TimelineFPS       int                `json:"timeline_fps"`
	SemanticRole      string             `json:"semantic_role,omitempty"`
	Overall           string             `json:"overall"`
	Evidence          []MaterialEvidence `json:"evidence,omitempty"`
	EvidenceTotal     int                `json:"evidence_total,omitempty"`
	EvidenceTruncated bool               `json:"evidence_truncated,omitempty"`
	AnalysisMethod    string             `json:"analysis_method,omitempty"`
	CandidateCutCount int                `json:"candidate_cut_count,omitempty"`
	VerifiedCutCount  int                `json:"verified_cut_count,omitempty"`
	Degraded          []string           `json:"degraded,omitempty"`
	UsageNote         string             `json:"usage_note,omitempty"`
}

type UnderstandResult struct {
	DraftID          string                         `json:"draft_id"`
	JobID            string                         `json:"job_id"`
	AssetIDs         []string                       `json:"asset_ids"`
	Status           string                         `json:"status"`
	Summaries        []MaterialUnderstandingSummary `json:"summaries,omitempty"`
	CacheHitAssetIDs []string                       `json:"cache_hit_asset_ids,omitempty"`
	AnalyzedAssetIDs []string                       `json:"analyzed_asset_ids,omitempty"`
	UsageNote        string                         `json:"usage_note,omitempty"`
}

type ShotSearchInput struct {
	Query             string   `json:"query,omitempty" jsonschema_description:"创作意图或画面语义，例如 夜晚火焰人物快速动作，适合高潮强拍"`
	AssetIDs          []string `json:"asset_ids,omitempty" jsonschema_description:"可选；只检索这些视频素材"`
	SemanticRoles     []string `json:"semantic_roles,omitempty" jsonschema_description:"可选；只检索 a_roll 或 b_roll 镜头，可同时传多个"`
	Tags              []string `json:"tags,omitempty" jsonschema_description:"可选；主体、动作、场景或氛围标签，任一匹配即可"`
	MinDurationFrames int      `json:"min_duration_frames,omitempty" jsonschema_description:"镜头源区间最短帧数"`
	MaxDurationFrames int      `json:"max_duration_frames,omitempty" jsonschema_description:"镜头源区间最长帧数；0 表示不限"`
	ExcludeUsed       bool     `json:"exclude_used,omitempty" jsonschema_description:"排除与当前时间线已使用源区间重叠的镜头"`
	Limit             int      `json:"limit,omitempty" jsonschema_description:"返回数量，默认 20，上限 100"`
}

type ShotCandidate struct {
	ShotID            string   `json:"shot_id"`
	AssetID           string   `json:"asset_id"`
	Filename          string   `json:"filename"`
	SourceStartFrame  int      `json:"source_start_frame"`
	SourceEndFrame    int      `json:"source_end_frame"`
	DurationFrames    int      `json:"duration_frames"`
	SemanticRole      string   `json:"semantic_role,omitempty"`
	Description       string   `json:"description,omitempty"`
	Tags              []string `json:"tags,omitempty"`
	Quality           string   `json:"quality,omitempty"`
	Subjects          []string `json:"subjects,omitempty"`
	Actions           []string `json:"actions,omitempty"`
	Setting           []string `json:"setting,omitempty"`
	ShotScale         string   `json:"shot_scale,omitempty"`
	Composition       string   `json:"composition,omitempty"`
	Lighting          []string `json:"lighting,omitempty"`
	Mood              []string `json:"mood,omitempty"`
	EditHints         []string `json:"edit_hints,omitempty"`
	Transcript        string   `json:"transcript,omitempty"`
	OverexposedRatio  *float64 `json:"overexposed_ratio,omitempty"`
	SharpnessScore    *float64 `json:"sharpness_score,omitempty"`
	BoundaryKind      string   `json:"boundary_kind,omitempty"`
	BoundaryVerified  bool     `json:"boundary_verified,omitempty"`
	MatchedQueryTerms []string `json:"matched_query_terms,omitempty"`
	MatchEvidence     []string `json:"match_evidence,omitempty"`
	SegmentScore      float64  `json:"segment_score,omitempty"`
	AssetScore        float64  `json:"asset_score,omitempty"`
	Score             float64  `json:"score"`
}

// ShotSearchUnderstandingCandidate exposes assets whose filename/path matches the
// current visual intent but which do not have shot-level understanding yet. It is
// deliberately not a shot: callers must understand the asset and search again
// before they can obtain a valid shot_id or source range.
type ShotSearchUnderstandingCandidate struct {
	AssetID           string   `json:"asset_id"`
	Filename          string   `json:"filename"`
	RelDir            string   `json:"rel_dir,omitempty"`
	DurationFrames    int      `json:"duration_frames,omitempty"`
	SemanticRole      string   `json:"semantic_role,omitempty"`
	MatchedQueryTerms []string `json:"matched_query_terms,omitempty"`
	MatchEvidence     []string `json:"match_evidence,omitempty"`
	Score             float64  `json:"score"`
}

type ShotSearchResult struct {
	Query                        string                             `json:"query,omitempty"`
	Shots                        []ShotCandidate                    `json:"shots"`
	TotalMatches                 int                                `json:"total_matches"`
	Truncated                    bool                               `json:"truncated"`
	MissingUnderstandingAssetIDs []string                           `json:"missing_understanding_asset_ids,omitempty"`
	UnderstandingCandidates      []ShotSearchUnderstandingCandidate `json:"understanding_candidates,omitempty"`
}

type AudioBeatAnalysisInput struct {
	AssetID        string `json:"asset_id" jsonschema:"required" jsonschema_description:"asset.list_assets 返回的 audio 素材 ID；带原声的视频不作为 BGM 节拍源"`
	MaxBeats       int    `json:"max_beats,omitempty" jsonschema_description:"最多返回的节拍点，默认 512，上限 2000"`
	WaveformPoints int    `json:"waveform_points,omitempty" jsonschema_description:"压缩 RMS 波形的最大采样点数，默认 96，可选范围 [16,256]"`
}

type AudioWaveformEnvelope struct {
	SampleIntervalFrames int     `json:"sample_interval_frames"`
	SampleFrames         []int   `json:"sample_frames" jsonschema_description:"与 samples 一一对应、按 timeline_fps 标尺表示的素材内 RMS 窗口起始帧，第 i 个响度值位于 sample_frames[i]；audio.analyze_beats 返回本次请求的完整压缩波形，WorldState 只常驻最多 24 点摘要"`
	Samples              []int   `json:"samples"`
	Encoding             string  `json:"encoding"`
	FloorDB              float64 `json:"floor_db"`
	CeilingDB            float64 `json:"ceiling_db"`
}

type AudioBeatAnalysisResult struct {
	AssetID             string                `json:"asset_id"`
	BPM                 float64               `json:"bpm"`
	TimelineFPS         int                   `json:"timeline_fps"`
	DurationFrames      int                   `json:"duration_frames"`
	BeatFrames          []int                 `json:"beat_frames"`
	StrongBeatFrames    []int                 `json:"strong_beat_frames"`
	DownbeatFrames      []int                 `json:"downbeat_frames"`
	EveryTwoBeatFrames  []int                 `json:"every_two_beat_frames"`
	EveryFourBeatFrames []int                 `json:"every_four_beat_frames"`
	BarPhase            int                   `json:"bar_phase"`
	AnalysisMethod      string                `json:"analysis_method"`
	Truncated           bool                  `json:"truncated"`
	PhaseNote           string                `json:"phase_note"`
	WaveformUsageNote   string                `json:"waveform_usage_note"`
	Waveform            AudioWaveformEnvelope `json:"waveform"`
}

type SpeechPauseAnalysisInput struct {
	AssetID           string  `json:"asset_id,omitempty" jsonschema_description:"音频或带音轨的视频素材 ID；与 timeline_clip_id 至少传一个"`
	TimelineClipID    string  `json:"timeline_clip_id,omitempty" jsonschema_description:"可选；时间线中的视频或音频 clip ID。传入后返回可直接用于 delete_range 的时间线帧范围"`
	ThresholdDB       float64 `json:"threshold_db,omitempty" jsonschema_description:"静音阈值 dB，默认 -35，范围 [-80,-10]"`
	MinPauseFrames    int     `json:"min_pause_frames,omitempty" jsonschema_description:"最短气口帧数，默认约 0.18 秒"`
	KeepEdgeFrames    int     `json:"keep_edge_frames,omitempty" jsonschema_description:"每个气口两侧保留帧数，默认约 0.06 秒，避免吃字"`
	MaxPauses         int     `json:"max_pauses,omitempty" jsonschema_description:"最多返回候选气口，默认 200，上限 1000"`
	IncludeBoundaries bool    `json:"include_boundaries,omitempty" jsonschema_description:"是否包含素材首尾静音，默认 false"`
}

type SpeechPauseCandidate struct {
	SourceStartFrame   int  `json:"source_start_frame"`
	SourceEndFrame     int  `json:"source_end_frame"`
	DeleteStartFrame   int  `json:"delete_start_frame"`
	DeleteEndFrame     int  `json:"delete_end_frame"`
	TimelineStartFrame *int `json:"timeline_start_frame,omitempty"`
	TimelineEndFrame   *int `json:"timeline_end_frame,omitempty"`
}

type SpeechPauseAnalysisResult struct {
	AssetID        string                 `json:"asset_id"`
	TimelineClipID string                 `json:"timeline_clip_id,omitempty"`
	TimelineFPS    int                    `json:"timeline_fps"`
	DurationFrames int                    `json:"duration_frames"`
	Pauses         []SpeechPauseCandidate `json:"pauses"`
	AnalysisMethod string                 `json:"analysis_method"`
	Truncated      bool                   `json:"truncated"`
	UsageNote      string                 `json:"usage_note"`
}

type SpeechInspectInput struct {
	AssetID          string `json:"asset_id,omitempty" jsonschema_description:"带声音的视频或音频素材 ID；与 timeline_clip_id 至少传一个"`
	TimelineClipID   string `json:"timeline_clip_id,omitempty" jsonschema_description:"当前时间线 A-roll clip ID；传入后返回映射后的时间线帧"`
	Query            string `json:"query,omitempty" jsonschema_description:"像 grep 一样检索台词语义；省略时返回时间顺序的口播索引"`
	SourceStartFrame *int   `json:"source_start_frame,omitempty" jsonschema_description:"可选源素材检索起点帧"`
	SourceEndFrame   *int   `json:"source_end_frame,omitempty" jsonschema_description:"可选源素材检索终点帧"`
	MaxUtterances    int    `json:"max_utterances,omitempty" jsonschema_description:"最多返回逐句证据，默认 80，上限 240"`
	IncludeWords     bool   `json:"include_words,omitempty" jsonschema_description:"需要精确检查或删除句内口误、卡壳、重复词时设为 true；返回稳定 word_id 与词级源帧"`
	MaxWords         int    `json:"max_words,omitempty" jsonschema_description:"include_words=true 时最多返回的词级证据，默认 400，上限 2000"`
	IncludePauses    *bool  `json:"include_pauses,omitempty" jsonschema_description:"是否返回带稳定 pause_id 的气口候选，默认 true"`
	MaxPauses        int    `json:"max_pauses,omitempty" jsonschema_description:"最多返回按可删除时长降序排列的气口证据，默认 24，上限 100；用源帧窗口可继续局部检索"`
	IncludeSimilar   *bool  `json:"include_similar,omitempty" jsonschema_description:"是否返回相似台词对的客观相似度证据，默认 true"`
	Language         string `json:"language,omitempty" jsonschema_description:"可选 ASR 语言，例如 zh、en；混合语言时省略"`
	ForceRefresh     bool   `json:"force_refresh,omitempty" jsonschema_description:"默认复用持久化转写；只有用户明确要求重新转写时设为 true"`
}

type SpeechWordEvidence struct {
	WordID             string `json:"word_id"`
	SourceStartFrame   int    `json:"source_start_frame"`
	SourceEndFrame     int    `json:"source_end_frame"`
	TimelineStartFrame *int   `json:"timeline_start_frame,omitempty"`
	TimelineEndFrame   *int   `json:"timeline_end_frame,omitempty"`
	Text               string `json:"text"`
	Punctuation        string `json:"punctuation,omitempty"`
}

type SpeechUtteranceEvidence struct {
	UtteranceID        string               `json:"utterance_id"`
	SourceStartFrame   int                  `json:"source_start_frame"`
	SourceEndFrame     int                  `json:"source_end_frame"`
	TimelineStartFrame *int                 `json:"timeline_start_frame,omitempty"`
	TimelineEndFrame   *int                 `json:"timeline_end_frame,omitempty"`
	Text               string               `json:"text"`
	Language           string               `json:"language,omitempty"`
	Emotion            string               `json:"emotion,omitempty"`
	Words              []SpeechWordEvidence `json:"words,omitempty"`
}

type SpeechPauseEvidence struct {
	PauseID                    string `json:"pause_id"`
	SourceStartFrame           int    `json:"source_start_frame"`
	SourceEndFrame             int    `json:"source_end_frame"`
	DeleteStartFrame           int    `json:"delete_start_frame"`
	DeleteEndFrame             int    `json:"delete_end_frame"`
	TimelineStartFrame         *int   `json:"timeline_start_frame,omitempty"`
	TimelineEndFrame           *int   `json:"timeline_end_frame,omitempty"`
	DurationFrames             int    `json:"duration_frames"`
	DeleteDurationFrames       int    `json:"delete_duration_frames"`
	DetectionMethod            string `json:"detection_method,omitempty"`
	PreviousText               string `json:"previous_text,omitempty"`
	NextText                   string `json:"next_text,omitempty"`
	PreviousWordID             string `json:"previous_word_id,omitempty"`
	NextWordID                 string `json:"next_word_id,omitempty"`
	PreviousContext            string `json:"previous_context,omitempty"`
	NextContext                string `json:"next_context,omitempty"`
	JoinedContext              string `json:"joined_context,omitempty"`
	PreviousContextStartWordID string `json:"previous_context_start_word_id,omitempty"`
	PreviousContextEndWordID   string `json:"previous_context_end_word_id,omitempty"`
	NextContextStartWordID     string `json:"next_context_start_word_id,omitempty"`
	NextContextEndWordID       string `json:"next_context_end_word_id,omitempty"`
}

type SpeechSimilarityEvidence struct {
	EarlierUtteranceID      string  `json:"earlier_utterance_id"`
	LaterUtteranceID        string  `json:"later_utterance_id"`
	EarlierEndUtteranceID   string  `json:"earlier_end_utterance_id,omitempty"`
	LaterEndUtteranceID     string  `json:"later_end_utterance_id,omitempty"`
	EarlierSourceStartFrame int     `json:"earlier_source_start_frame"`
	EarlierSourceEndFrame   int     `json:"earlier_source_end_frame"`
	LaterSourceStartFrame   int     `json:"later_source_start_frame"`
	LaterSourceEndFrame     int     `json:"later_source_end_frame"`
	EarlierText             string  `json:"earlier_text"`
	LaterText               string  `json:"later_text"`
	Similarity              float64 `json:"similarity"`
	MatchedCharacters       int     `json:"matched_characters,omitempty"`
	Method                  string  `json:"method"`
	Evidence                string  `json:"evidence"`
}

type SpeechRepetitionEvidence struct {
	RepetitionID            string `json:"repetition_id"`
	UtteranceID             string `json:"utterance_id"`
	Kind                    string `json:"kind"`
	EarlierStartWordID      string `json:"earlier_start_word_id"`
	EarlierEndWordID        string `json:"earlier_end_word_id"`
	LaterStartWordID        string `json:"later_start_word_id"`
	LaterEndWordID          string `json:"later_end_word_id"`
	EarlierSourceStartFrame int    `json:"earlier_source_start_frame"`
	EarlierSourceEndFrame   int    `json:"earlier_source_end_frame"`
	LaterSourceStartFrame   int    `json:"later_source_start_frame"`
	LaterSourceEndFrame     int    `json:"later_source_end_frame"`
	EarlierText             string `json:"earlier_text"`
	LaterText               string `json:"later_text"`
	MatchedText             string `json:"matched_text,omitempty"`
	MatchedCharacters       int    `json:"matched_characters"`
	ContextText             string `json:"context_text"`
	Evidence                string `json:"evidence"`
}

type SpeechFragmentEvidence struct {
	FragmentID                string `json:"fragment_id"`
	UtteranceID               string `json:"utterance_id"`
	PauseID                   string `json:"pause_id"`
	Kind                      string `json:"kind"`
	StartWordID               string `json:"start_word_id"`
	EndWordID                 string `json:"end_word_id"`
	SourceStartFrame          int    `json:"source_start_frame"`
	SourceEndFrame            int    `json:"source_end_frame"`
	DurationFrames            int    `json:"duration_frames"`
	Text                      string `json:"text"`
	PreviousContext           string `json:"previous_context,omitempty"`
	NextContext               string `json:"next_context"`
	JoinedContext             string `json:"joined_context"`
	PauseDurationFrames       int    `json:"pause_duration_frames"`
	NextContextStartWordID    string `json:"next_context_start_word_id,omitempty"`
	NextContextEndWordID      string `json:"next_context_end_word_id,omitempty"`
	RestartAnchorText         string `json:"restart_anchor_text,omitempty"`
	MatchedEarlierUtteranceID string `json:"matched_earlier_utterance_id,omitempty"`
	MatchedEarlierText        string `json:"matched_earlier_text,omitempty"`
	Evidence                  string `json:"evidence"`
}

type SpeechInspectResult struct {
	TranscriptID            string                     `json:"transcript_id"`
	AssetID                 string                     `json:"asset_id"`
	TimelineClipID          string                     `json:"timeline_clip_id,omitempty"`
	TimelineFPS             int                        `json:"timeline_fps"`
	ProviderID              string                     `json:"provider_id"`
	CacheHit                bool                       `json:"cache_hit"`
	Repetitions             []SpeechRepetitionEvidence `json:"intra_utterance_repetitions,omitempty"`
	RepetitionTotal         int                        `json:"repetition_total,omitempty"`
	RepetitionsTruncated    bool                       `json:"repetitions_truncated,omitempty"`
	ShortFragments          []SpeechFragmentEvidence   `json:"short_speech_fragments,omitempty"`
	ShortFragmentTotal      int                        `json:"short_fragment_total,omitempty"`
	ShortFragmentsTruncated bool                       `json:"short_fragments_truncated,omitempty"`
	Pauses                  []SpeechPauseEvidence      `json:"pauses,omitempty"`
	PauseTotal              int                        `json:"pause_total,omitempty"`
	PausesTruncated         bool                       `json:"pauses_truncated,omitempty"`
	SimilarPairs            []SpeechSimilarityEvidence `json:"similar_pairs,omitempty"`
	Utterances              []SpeechUtteranceEvidence  `json:"utterances"`
	UtteranceTotal          int                        `json:"utterance_total"`
	WordTotal               int                        `json:"word_total,omitempty"`
	WordsTruncated          bool                       `json:"words_truncated,omitempty"`
	Truncated               bool                       `json:"truncated"`
	UsageNote               string                     `json:"usage_note"`
}

type DecisionOptionInput struct {
	OptionID    string `json:"option_id" jsonschema:"required" jsonschema_description:"稳定选项 ID；用户回答后会原样回传，不要用展示文案充当 ID"`
	Label       string `json:"label" jsonschema:"required" jsonschema_description:"决策卡上显示给用户的简体中文选项名称"`
	Description string `json:"description,omitempty" jsonschema_description:"可选的简体中文影响或取舍说明，帮助用户理解该选项"`
}

type AskUserInput struct {
	Question      string                `json:"question" jsonschema:"required" jsonschema_description:"只在缺少关键决策且无法安全推断时显示给用户的简体中文问题；只聚焦一个会实质改变成片目标的核心分歧，不得附带首剪方案、EDL 或细节审批清单"`
	Options       []DecisionOptionInput `json:"options,omitempty" jsonschema_description:"可选的结构化选择，最多三个实质不同的方向；不要提供确认/修改首剪方案这类细节审批选项"`
	AllowFreeText *bool                 `json:"allow_free_text,omitempty" jsonschema_description:"是否允许用户补充自由文本，默认 true"`
	Blocking      *bool                 `json:"blocking,omitempty" jsonschema_description:"是否阻塞后续工具执行，默认 true；false 只收集非阻塞偏好，不应停止当前任务"`
	DecisionType  string                `json:"decision_type" jsonschema:"required" jsonschema_description:"模型主动提问只能传 critical，表示缺失信息会让成片目标产生实质冲突且无法安全推断；可逆剪辑细节必须自主决定。其他确认由专用策略工具创建"`
}

type DecisionAnswerInput struct {
	DecisionID string         `json:"decision_id" jsonschema:"required" jsonschema_description:"已有待答决策的 decision_id；不能回答本回合由 interaction.ask_user 刚创建的决策，必须等待真实用户"`
	OptionID   string         `json:"option_id,omitempty" jsonschema_description:"从该决策 options 中选择的 option_id；与用户自由文本至少提供一项"`
	FreeText   string         `json:"free_text,omitempty" jsonschema_description:"用户明确提供的自由文本答案；不得由模型代替用户编造"`
	Payload    map[string]any `json:"payload,omitempty" jsonschema_description:"可选结构化补充数据；仅透传真实用户或受信任上游已给出的字段"`
}

type ContentPlanFrameRange struct {
	StartFrame int `json:"start_frame" jsonschema_description:"验收区间起始时间线帧，包含"`
	EndFrame   int `json:"end_frame" jsonschema_description:"验收区间结束时间线帧，不包含且必须大于 start_frame"`
}

type ContentPlanContract struct {
	TargetDurationFrames    int                     `json:"target_duration_frames,omitempty" jsonschema_description:"目标成片总帧数；大于 0 时编辑后自动核对"`
	DurationToleranceFrames *int                    `json:"duration_tolerance_frames,omitempty" jsonschema_description:"目标时长允许误差帧数；省略时为 timeline_fps 的一半，显式传 0 表示必须精确命中"`
	MustKeepUtteranceIDs    []string                `json:"must_keep_utterance_ids,omitempty" jsonschema_description:"speech.inspect 返回、成片必须完整保留的 utterance_id"`
	BrollCoverageRanges     []ContentPlanFrameRange `json:"broll_coverage_ranges,omitempty" jsonschema_description:"必须由 visual_overlay B-roll 完整覆盖的时间线帧区间"`
	MinOnBeatRatio          *float64                `json:"min_on_beat_ratio,omitempty" jsonschema_description:"画面切点落在真实 beat_grid 的最低比例，范围 0 到 1"`
	Rhythm                  string                  `json:"rhythm,omitempty" jsonschema_description:"创作节奏意图，例如舒缓、均衡、紧凑；作为计划语义保留，数值验收使用切点密度字段"`
	MinCutDensityPerMinute  *float64                `json:"min_cut_density_per_minute,omitempty" jsonschema_description:"每分钟画面切点数下限"`
	MaxCutDensityPerMinute  *float64                `json:"max_cut_density_per_minute,omitempty" jsonschema_description:"每分钟画面切点数上限"`
}

type PlanUpdateInput struct {
	Plan     map[string]any       `json:"plan" jsonschema:"required" jsonschema_description:"要写入持久创作计划本的 JSON 对象；默认按 RFC 7396 增量合并，值为 null 时删除对应键"`
	Contract *ContentPlanContract `json:"contract,omitempty" jsonschema_description:"可执行验收合同；传入后写入 content_plan.contract，后续编辑与预览会自动核对"`
	Reset    *bool                `json:"reset,omitempty" jsonschema_description:"设为 true 时先清空现有计划，再按 RFC 7396 写入 plan；对象属性 null 仍表示删除；默认 false"`
}

type ComposeClip struct {
	AssetID          string `json:"asset_id" jsonschema:"required" jsonschema_description:"asset.list_assets 返回的 video 或 image 素材 ID"`
	SourceStartFrame int    `json:"source_start_frame" jsonschema_description:"素材入点整数帧，默认 0，必须小于 source_end_frame"`
	SourceEndFrame   int    `json:"source_end_frame" jsonschema:"required" jsonschema_description:"素材出点整数帧，不得超过素材 duration_frames"`
	Role             string `json:"role" jsonschema:"required" jsonschema_description:"片段视觉角色：a_roll 作为主叙事，b_roll 作为补充画面"`
}

type ComposeInitialInput struct {
	Clips []ComposeClip `json:"clips" jsonschema:"required" jsonschema_description:"按成片顺序排列的主视觉片段；根据用户目标和素材证据自主组装可回滚的初版，不要要求用户审批 EDL"`
}

type TimelinePatchInput struct {
	Op TimelineOp `json:"op" jsonschema:"required" jsonschema_description:"从 op.oneOf 选择一种扁平补丁，按所选 kind 提供字段"`
}

type TimelinePatchBatchInput struct {
	Ops []TimelineOp `json:"ops" jsonschema:"required" jsonschema_description:"按顺序原子应用的时间线语义补丁；每项从 oneOf 选择 kind 和字段；卡点剪辑或同时修改多个 clip 时优先用此工具，整批只写入一次当前时间线"`
}

type TalkingHeadBrollAssignment struct {
	ShotID           string `json:"shot_id" jsonschema:"required" jsonschema_description:"media.search_shots 返回的 b_roll shot_id"`
	StartUtteranceID string `json:"start_utterance_id,omitempty" jsonschema_description:"B-roll 覆盖语义的起始 utterance_id；与 start_word_id 二选一，且该句在本次所有 remove 决定展开后必须仍被保留"`
	EndUtteranceID   string `json:"end_utterance_id,omitempty" jsonschema_description:"结束 utterance_id；省略时只覆盖起始句，且不能同时属于本次删除范围"`
	AnchorText       string `json:"anchor_text,omitempty" jsonschema_description:"从 speech.inspect 返回的 utterance 原文逐字复制的唯一连续短语；其中每个词在本次 remove_word_ranges、repetition_decisions、short_fragment_decisions 展开后都必须保留，不能把将删除的卡壳或重说词放进锚点"`
	StartWordID      string `json:"start_word_id,omitempty" jsonschema_description:"词级 B-roll 语义锚点起始 word_id；与 start_utterance_id 二选一，且该词不能被本次任何删除决定覆盖"`
	EndWordID        string `json:"end_word_id,omitempty" jsonschema_description:"词级语义锚点结束 word_id；省略时只覆盖起始词，整段连续 word_id 都必须在本次编辑后保留"`
}

type TalkingHeadWordRange struct {
	StartWordID string `json:"start_word_id" jsonschema:"required" jsonschema_description:"speech.inspect(include_words=true) 返回的起始 word_id"`
	EndWordID   string `json:"end_word_id,omitempty" jsonschema_description:"连续删除范围的结束 word_id；省略时只删除起始词"`
}

type TalkingHeadFragmentDecision struct {
	FragmentID string `json:"fragment_id" jsonschema:"required" jsonschema_description:"speech.inspect.short_speech_fragments 返回的 fragment_id"`
	Action     string `json:"action" jsonschema:"required" jsonschema_description:"模型结合 text、previous_context、next_context、joined_context 后的明确决定：remove 或 preserve"`
	Reason     string `json:"reason,omitempty" jsonschema_description:"保留 restart_prefix_before_repeated_take 时必填：至少 20 字，原样引用 fragment.text 与 restart_anchor_text，并解释拼接语义；其他决定可简短说明"`
}

type TalkingHeadRepetitionDecision struct {
	RepetitionID string `json:"repetition_id" jsonschema:"required" jsonschema_description:"speech.inspect.intra_utterance_repetitions 返回的 repetition_id"`
	Action       string `json:"action" jsonschema:"required" jsonschema_description:"模型结合 context_text 后的明确决定：remove_earlier、remove_later 或 preserve；工具不会替模型判断重复词是卡壳、数字拆词还是正常叠词"`
	Reason       string `json:"reason,omitempty" jsonschema_description:"简述结合上下文选择删除前一段、后一段或保留的理由"`
}

type TalkingHeadPauseDecision struct {
	PauseID string `json:"pause_id" jsonschema:"required" jsonschema_description:"speech.inspect.pauses 返回的稳定 pause_id"`
	Action  string `json:"action" jsonschema:"required" jsonschema_description:"模型结合 previous_context、next_context 与 joined_context 后的明确决定：remove 或 preserve；工具不会替模型判断是气口还是正常表达停顿"`
	Reason  string `json:"reason,omitempty" jsonschema_description:"简述为何删除该气口，或为何它属于应保留的正常表达停顿"`
}

type TalkingHeadEditInput struct {
	ARollTimelineClipID    string                          `json:"a_roll_timeline_clip_id" jsonschema:"required" jsonschema_description:"timeline.inspect 返回的 A-roll 主视频 clip ID"`
	RemoveUtteranceIDs     []string                        `json:"remove_utterance_ids,omitempty" jsonschema_description:"模型根据 ASR 语义自行判断后选择删除的 utterance_id，例如语义重复或口误"`
	RemoveWordRanges       []TalkingHeadWordRange          `json:"remove_word_ranges,omitempty" jsonschema_description:"模型根据词级 ASR 证据选择的连续句内删除范围；用于卡壳、重复词或半句重说，不能猜 word_id"`
	RemovePauseIDs         []string                        `json:"remove_pause_ids,omitempty" jsonschema_description:"模型根据 speech.inspect 证据自行选择删除的 pause_id"`
	PauseDecisions         []TalkingHeadPauseDecision      `json:"pause_decisions,omitempty" jsonschema_description:"对 speech.inspect 返回的显著气口候选逐项给出 remove/preserve 决定；remove 会自动加入删除项，preserve 只记录模型判断"`
	RepetitionDecisions    []TalkingHeadRepetitionDecision `json:"repetition_decisions,omitempty" jsonschema_description:"对 speech.inspect 返回的每个 intra_utterance_repetitions 候选一次性给出 remove_earlier/remove_later/preserve 决定；删除动作会自动解析候选自带的精确连续 word_id 范围"`
	ShortFragmentDecisions []TalkingHeadFragmentDecision   `json:"short_fragment_decisions,omitempty" jsonschema_description:"对 speech.inspect 返回的每个 short_speech_fragments 候选一次性给出 remove/preserve 决定；remove 会自动解析为该候选的精确连续 word_id 范围，避免在失败后逐个补参数"`
	BrollAssignments       []TalkingHeadBrollAssignment    `json:"b_roll_assignments,omitempty" jsonschema_description:"模型自行选择的台词语义与 B-roll 镜头对应关系"`
}

type BeatRecutSFXInput struct {
	AssetID        string   `json:"asset_id" jsonschema:"required" jsonschema_description:"作为点缀的音效素材 ID"`
	StartFrame     *int     `json:"start_frame" jsonschema:"required" jsonschema_description:"音效在时间线上的起始整数帧；由模型结合波形、拍点和创作意图自主选择"`
	DurationFrames int      `json:"duration_frames" jsonschema:"required" jsonschema_description:"短音效时长，通常为 30 到 60 帧"`
	GainDB         *float64 `json:"gain_db,omitempty" jsonschema_description:"音效增益，省略时为 -12 dB"`
}

type TimelineBeatRecutInput struct {
	BGMAssetID           string             `json:"bgm_asset_id,omitempty" jsonschema_description:"BGM 音频素材 ID；新建、重建或当前时间线没有 BGM 时传此字段。可从 audio.analyze_beats 的 asset_id 原样取得"`
	BGMTimelineClipID    string             `json:"bgm_timeline_clip_id,omitempty" jsonschema_description:"兼容已有时间线局部重剪；timeline.inspect 返回的 BGM clip ID。完整重剪优先传 bgm_asset_id"`
	TargetDurationFrames int                `json:"target_duration_frames,omitempty" jsonschema_description:"目标成片总帧数。用户给出秒数时乘以 timeline_fps；要求覆盖整首 BGM 时可省略并设置 cover_entire_bgm=true"`
	CoverEntireBGM       bool               `json:"cover_entire_bgm,omitempty" jsonschema_description:"用户要求覆盖整首音乐时必须为 true；工具会以 BGM 的完整可用帧数重建主视频并铺满音乐"`
	VideoAssetIDs        []string           `json:"video_asset_ids,omitempty" jsonschema_description:"可选的主视频素材顺序；省略时先保留当前主视频素材顺序，再自动补入草稿内其余可用视频"`
	UseAllVideoAssets    bool               `json:"use_all_video_assets,omitempty" jsonschema_description:"用户要求每个/全部视频素材都用上时必须为 true；语义是每个素材至少一次，额外片段可从同一素材的其他不重叠镜头区间取得"`
	ShotIDs              []string           `json:"shot_ids,omitempty" jsonschema_description:"media.search_shots 返回的有序镜头 ID；与 cut_frames 一一对应，工具会从持久化理解摘要解析精确源帧，禁止猜测"`
	CutFrames            []int              `json:"cut_frames,omitempty" jsonschema_description:"可选的累计结束帧；完整重剪会逐项校验其属于真实 beat_frames。可多于视频素材数，同一素材会使用不同且不重叠的源区间；传 shot_ids 时数量必须一致"`
	SFX                  *BeatRecutSFXInput `json:"sfx,omitempty" jsonschema_description:"可选的音效点缀；传入时由模型显式选择 start_frame，工具只校验范围、时长、增益与素材合法性，并始终写入独立 sfx 轨"`
}

type TimelineValidateInput struct{}

type TimelineInspectInput struct{}

type RenderPreviewInput struct {
	Orientation string `json:"orientation,omitempty" jsonschema_description:"成片画幅方向：auto、portrait 或 landscape；默认 auto"`
}

type RenderFinalInput struct {
	Orientation string `json:"orientation,omitempty" jsonschema_description:"成片画幅方向：auto、portrait 或 landscape；默认 auto"`
}

type RenderStatusInput struct{}

type RenderInspectInput struct {
	PreviewID string   `json:"preview_id" jsonschema:"required" jsonschema_description:"render.preview 成功产物返回的 preview_id"`
	Checks    []string `json:"checks,omitempty" jsonschema_description:"确定性检查项：decode、black、freeze、silence、loudness，省略时执行这五项；需要 contact sheet 视觉检查时额外传 visual"`
}

type PreviewInspectionResult struct {
	Summary            string                   `json:"summary"`
	Degraded           bool                     `json:"degraded"`
	Issues             []map[string]interface{} `json:"issues"`
	VisualFrameCount   int                      `json:"visual_frame_count,omitempty"`
	VisualLatencyMS    int64                    `json:"visual_latency_ms,omitempty"`
	VisualPromptTokens int                      `json:"visual_prompt_tokens,omitempty"`
	VisualTotalTokens  int                      `json:"visual_total_tokens,omitempty"`
}

type ConfirmActionInput struct {
	Question  string         `json:"question" jsonschema:"required" jsonschema_description:"向用户说明破坏性动作与影响的简体中文确认问题"`
	ToolName  string         `json:"tool_name" jsonschema:"required" jsonschema_description:"用户确认后才允许重放的已注册工具名"`
	Arguments map[string]any `json:"arguments" jsonschema:"required" jsonschema_description:"用户确认后原样重放给目标工具的参数对象"`
}
