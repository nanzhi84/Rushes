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

type Reporter func(ctx context.Context, name, phase string, input, output any, err error)

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

type DetectShotsInput struct {
	AssetID          string `json:"asset_id" jsonschema:"required" jsonschema_description:"asset.list_assets 返回的单个 video 素材 ID；多素材必须由模型并行调用本工具"`
	Depth            string `json:"depth,omitempty" jsonschema_description:"scan 做低成本镜头检测，deep 做逐镜头深度理解；默认 scan；调用会在当前 turn 内等待终态"`
	Focus            string `json:"focus,omitempty" jsonschema_description:"可选创作关注点，例如产品特写、人物动作或可用于高潮的镜头；会进入视觉分析提示与缓存键"`
	MaxStepsPerAsset int    `json:"max_steps_per_asset,omitempty" jsonschema_description:"每个素材的最大分析步骤数；0 使用服务端默认值，数值越大成本和延迟越高"`
	ForceRefresh     bool   `json:"force_refresh,omitempty" jsonschema_description:"仅当用户明确要求重新分析时设为 true；默认复用相同素材与参数的持久化结果"`
	RefreshNonce     string `json:"refresh_nonce,omitempty" jsonschema_description:"仅当用户在旧强制任务终态后明确要求再次重跑完全相同的分析时提供新的短标识；同一标识重复调用仍幂等复用同一 job"`
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

type DetectShotsResult struct {
	DraftID   string                        `json:"draft_id"`
	JobID     string                        `json:"job_id,omitempty"`
	AssetID   string                        `json:"asset_id"`
	Status    string                        `json:"status"`
	Data      map[string]any                `json:"data,omitempty"`
	Summary   *MaterialUnderstandingSummary `json:"summary,omitempty"`
	CacheHit  bool                          `json:"cache_hit"`
	Analyzed  bool                          `json:"analyzed"`
	UsageNote string                        `json:"usage_note,omitempty"`
}

type ShotSearchInput struct {
	Query             string   `json:"query,omitempty" jsonschema_description:"创作意图或画面语义，例如 夜晚火焰人物快速动作，适合高潮强拍"`
	TopK              int      `json:"top_k,omitempty" jsonschema_description:"返回候选数，默认 12；精确搜索约 5，宽泛探索约 20；范围 1 到 30"`
	AssetIDs          []string `json:"asset_ids,omitempty" jsonschema_description:"可选；冻结并只检索这些当前草稿内可用的视频素材；省略时冻结全部可用视频素材"`
	SemanticRoles     []string `json:"semantic_roles,omitempty" jsonschema_description:"可选；只检索 a_roll 或 b_roll 镜头，可同时传多个"`
	Tags              []string `json:"tags,omitempty" jsonschema_description:"可选；主体、动作、场景或氛围标签，至少一项须匹配"`
	MinDurationFrames int      `json:"min_duration_frames,omitempty" jsonschema_description:"镜头权威源区间的最短帧数"`
	MaxDurationFrames int      `json:"max_duration_frames,omitempty" jsonschema_description:"镜头权威源区间的最长帧数；0 表示不限"`
	ExcludeUsed       bool     `json:"exclude_used,omitempty" jsonschema_description:"排除与当前时间线已使用源区间重叠的镜头"`
}

type ShotCandidate struct {
	IndexSnapshotID  string   `json:"index_snapshot_id"`
	ShotID           string   `json:"shot_id"`
	AssetID          string   `json:"asset_id"`
	Filename         string   `json:"filename"`
	SourceStartFrame int      `json:"source_start_frame"`
	SourceEndFrame   int      `json:"source_end_frame"`
	DurationFrames   int      `json:"duration_frames"`
	BoundaryVersion  int      `json:"boundary_version"`
	SemanticRole     string   `json:"semantic_role,omitempty"`
	Description      string   `json:"description"`
	Tags             []string `json:"tags,omitempty"`
	Quality          string   `json:"quality,omitempty"`
	Subjects         []string `json:"subjects,omitempty"`
	Actions          []string `json:"actions,omitempty"`
	Setting          []string `json:"setting,omitempty"`
	ShotScale        string   `json:"shot_scale,omitempty"`
	Composition      string   `json:"composition,omitempty"`
	Lighting         []string `json:"lighting,omitempty"`
	Mood             []string `json:"mood,omitempty"`
	EditHints        []string `json:"edit_hints,omitempty"`
	DeepCoverage     []string `json:"deep_coverage,omitempty"`
	MatchedTerms     []string `json:"matched_terms,omitempty"`
	MatchEvidence    []string `json:"match_evidence,omitempty"`
	Score            float64  `json:"score"`
}

type ShotSearchResult struct {
	Status             string          `json:"status"`
	ErrorCode          string          `json:"error_code,omitempty"`
	Recovery           string          `json:"recovery,omitempty"`
	Query              string          `json:"query,omitempty"`
	IndexSnapshotID    string          `json:"index_snapshot_id,omitempty"`
	SynonymVersion     string          `json:"synonym_version"`
	FrozenAssetIDs     []string        `json:"frozen_asset_ids"`
	ReadyAssetIDs      []string        `json:"ready_asset_ids,omitempty"`
	PendingAssetIDs    []string        `json:"pending_asset_ids,omitempty"`
	FailedAssetIDs     []string        `json:"failed_asset_ids,omitempty"`
	SearchReady        bool            `json:"search_ready"`
	WaitDurationMS     int64           `json:"wait_duration_ms,omitempty"`
	Shots              []ShotCandidate `json:"shots"`
	TotalMatches       int             `json:"total_matches"`
	ReturnedCandidates int             `json:"returned_candidates"`
	Truncated          bool            `json:"truncated"`
}

type ShotRefInput struct {
	AssetID string `json:"asset_id" jsonschema:"required" jsonschema_description:"shot.search 返回的精确素材 ID"`
	ShotID  string `json:"shot_id" jsonschema:"required" jsonschema_description:"shot.search 返回的持久 shot_id；不要传源帧范围"`
}

type ShotDeepSearchInput struct {
	Query           string         `json:"query" jsonschema:"required" jsonschema_description:"需要新增帧核验的具体画面问题或创作意图"`
	IndexSnapshotID string         `json:"index_snapshot_id" jsonschema:"required" jsonschema_description:"同一次 shot.search 返回的冻结 index_snapshot_id"`
	CandidateShots  []ShotRefInput `json:"candidate_shots" jsonschema:"required" jsonschema_description:"只接受 1 到 8 个精确 ShotRef；每项同时包含 asset_id 和 shot_id，不传源帧范围"`
	Requirements    []string       `json:"requirements,omitempty" jsonschema_description:"必须被新增帧证据支持的条件；被反驳时确定性 reject"`
	Exclusions      []string       `json:"exclusions,omitempty" jsonschema_description:"观察到任一排除条件时确定性 reject"`
	Preferences     []string       `json:"preferences,omitempty" jsonschema_description:"只影响候选排序，不做硬过滤"`
	ReturnTopK      int            `json:"return_top_k,omitempty" jsonschema_description:"返回候选数，默认全部；范围 1 到 candidate_shots 数量"`
}

type ShotDeepFrameEvidence struct {
	FrameID     string `json:"frame_id"`
	SourceFrame int    `json:"source_frame"`
	TimestampMS int64  `json:"timestamp_ms"`
	Position    string `json:"position"`
	ObjectHash  string `json:"object_hash"`
	ObjectSize  int64  `json:"object_size"`
	NewlyAdded  bool   `json:"newly_added"`
}

type ShotDeepCriterionEvidence struct {
	Criterion   string   `json:"criterion"`
	Status      string   `json:"status" jsonschema_description:"observed、refuted 或 uncertain"`
	Observation string   `json:"observation"`
	FrameIDs    []string `json:"frame_ids"`
}

type ShotDeepCandidate struct {
	IndexSnapshotID  string                      `json:"index_snapshot_id"`
	AssetID          string                      `json:"asset_id"`
	ShotID           string                      `json:"shot_id"`
	SourceStartFrame int                         `json:"source_start_frame"`
	SourceEndFrame   int                         `json:"source_end_frame"`
	BoundaryVersion  int                         `json:"boundary_version"`
	Verification     string                      `json:"verification" jsonschema_description:"match、reject、partial 或 uncertain"`
	Score            float64                     `json:"score"`
	Requirements     []ShotDeepCriterionEvidence `json:"requirements"`
	Exclusions       []ShotDeepCriterionEvidence `json:"exclusions"`
	Preferences      []ShotDeepCriterionEvidence `json:"preferences"`
	Observations     []string                    `json:"observations"`
	FrameEvidence    []ShotDeepFrameEvidence     `json:"frame_evidence"`
	DeepCoverage     []string                    `json:"deep_coverage"`
}

type ShotDeepSearchResult struct {
	Status                string              `json:"status"`
	ErrorCode             string              `json:"error_code,omitempty"`
	Message               string              `json:"message,omitempty"`
	Recovery              string              `json:"recovery,omitempty"`
	Query                 string              `json:"query,omitempty"`
	IndexSnapshotID       string              `json:"index_snapshot_id,omitempty"`
	AnalyzerVersion       string              `json:"analyzer_version,omitempty"`
	InvalidCandidateShots []ShotRefInput      `json:"invalid_candidate_shots,omitempty"`
	Candidates            []ShotDeepCandidate `json:"candidates"`
	TotalCandidates       int                 `json:"total_candidates"`
	ReturnedCandidates    int                 `json:"returned_candidates"`
	NewFrameCount         int                 `json:"new_frame_count"`
	ReusedFrameCount      int                 `json:"reused_frame_count"`
	CacheHit              bool                `json:"cache_hit"`
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
	AnalysisID          string                `json:"analysis_id"`
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
	CacheHit            bool                  `json:"cache_hit"`
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
	AnalysisID     string                 `json:"analysis_id"`
	AssetID        string                 `json:"asset_id"`
	TimelineClipID string                 `json:"timeline_clip_id,omitempty"`
	TimelineFPS    int                    `json:"timeline_fps"`
	DurationFrames int                    `json:"duration_frames"`
	Pauses         []SpeechPauseCandidate `json:"pauses"`
	AnalysisMethod string                 `json:"analysis_method"`
	Truncated      bool                   `json:"truncated"`
	UsageNote      string                 `json:"usage_note"`
	CacheHit       bool                   `json:"cache_hit"`
}

type SpeechTranscribeInput struct {
	AssetID      string `json:"asset_id" jsonschema:"required" jsonschema_description:"asset.list_assets 返回的单个 video 或 audio 素材 ID；多素材必须由模型并行调用本工具"`
	Language     string `json:"language,omitempty" jsonschema_description:"可选 ASR 语言，例如 zh、en；混合语言时省略"`
	ForceRefresh bool   `json:"force_refresh,omitempty" jsonschema_description:"默认复用持久化转写；只有用户明确要求重新转写时设为 true"`
}

type SpeechTranscribeResult struct {
	TranscriptID   string `json:"transcript_id"`
	AssetID        string `json:"asset_id"`
	TimelineFPS    int    `json:"timeline_fps"`
	ProviderID     string `json:"provider_id"`
	CacheHit       bool   `json:"cache_hit"`
	UtteranceTotal int    `json:"utterance_total"`
	WordTotal      int    `json:"word_total"`
	PauseTotal     int    `json:"pause_total"`
}

type SpeechSearchInput struct {
	AssetID          string `json:"asset_id,omitempty" jsonschema_description:"带声音的视频或音频素材 ID；与 timeline_clip_id 至少传一个"`
	TimelineClipID   string `json:"timeline_clip_id,omitempty" jsonschema_description:"CurrentTimelineView 返回、覆盖目标 source range 的当前 A-roll clip ID；传入后确定性返回映射后的时间线帧，供波纹删除后锚定 B-roll"`
	Query            string `json:"query,omitempty" jsonschema_description:"像 grep 一样检索台词语义；省略时返回时间顺序的口播索引"`
	SourceStartFrame *int   `json:"source_start_frame,omitempty" jsonschema_description:"可选源素材检索起点帧"`
	SourceEndFrame   *int   `json:"source_end_frame,omitempty" jsonschema_description:"可选源素材检索终点帧"`
	MaxUtterances    int    `json:"max_utterances,omitempty" jsonschema_description:"最多返回逐句证据，默认 80，上限 240"`
	IncludeWords     bool   `json:"include_words,omitempty" jsonschema_description:"需要精确检查或删除句内口误、卡壳、重复词时设为 true；返回稳定 word_id 与词级源帧"`
	MaxWords         int    `json:"max_words,omitempty" jsonschema_description:"include_words=true 时最多返回的词级证据，默认 400，上限 2000"`
	IncludePauses    *bool  `json:"include_pauses,omitempty" jsonschema_description:"是否返回带稳定 pause_id 的气口候选，默认 true"`
	MaxPauses        int    `json:"max_pauses,omitempty" jsonschema_description:"最多返回按可删除时长降序排列的气口证据，默认 24，上限 100；用源帧窗口可继续局部检索"`
	IncludeSimilar   *bool  `json:"include_similar,omitempty" jsonschema_description:"是否返回相似台词对的客观相似度证据，默认 true"`
}

type SpeechWordEvidence struct {
	WordID             string `json:"word_id"`
	SourceStartFrame   int    `json:"source_start_frame"`
	SourceEndFrame     int    `json:"source_end_frame"`
	TimelineStartFrame *int   `json:"timeline_start_frame,omitempty"`
	TimelineEndFrame   *int   `json:"timeline_end_frame,omitempty"`
	Text               string `json:"text"`
	Punctuation        string `json:"punctuation,omitempty"`
	Clamped            bool   `json:"clamped,omitempty"`
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
	Clamped            bool                 `json:"clamped,omitempty"`
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
	Clamped                    bool   `json:"clamped,omitempty"`
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

type SpeechSearchResult struct {
	Status                  string                     `json:"status"`
	ErrorCode               string                     `json:"error_code,omitempty"`
	Recovery                string                     `json:"recovery,omitempty"`
	TranscriptID            string                     `json:"transcript_id"`
	AssetID                 string                     `json:"asset_id"`
	TimelineClipID          string                     `json:"timeline_clip_id,omitempty"`
	TimelineFPS             int                        `json:"timeline_fps"`
	ProviderID              string                     `json:"provider_id"`
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
	MustKeepUtteranceIDs    []string                `json:"must_keep_utterance_ids,omitempty" jsonschema_description:"speech.search 返回、成片必须完整保留的 utterance_id"`
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

type MemoryEntryInput struct {
	Key       string `json:"key" jsonschema:"required" jsonschema_description:"稳定语义主键，匹配 [a-z0-9_]{2,40}，例如 pacing、subtitle_style；同键覆盖旧记忆"`
	Kind      string `json:"kind" jsonschema:"required" jsonschema_description:"preference 表示长期偏好，correction 表示用户纠正，habit 表示稳定使用习惯"`
	Statement string `json:"statement" jsonschema:"required" jsonschema_description:"一句简体中文陈述用户跨项目稳定的偏好、纠正或习惯，不超过 200 字；只能写当前用户明确表达过的内容，不得写模型自己的创作判断"`
	// EvidenceQuote 由模型提供、由 reducer 事务内比对，不是系统权威字段，故不触发 PolicyGate。
	EvidenceQuote string `json:"evidence_quote" jsonschema:"required" jsonschema_description:"从当前这条用户消息或决策回答里逐字摘录的原文片段，用于佐证该记忆确有用户依据；必须是原文连续子串，改写、拼接或与本条陈述无关的摘录都会被拒绝"`
}

type MemorySetInput struct {
	Entries []MemoryEntryInput `json:"entries" jsonschema:"required" jsonschema_description:"要写入或覆盖的长期记忆，单次最多 8 条；一次性项目要求不要入库"`
}

type MemoryRemoveInput struct {
	Keys []string `json:"keys" jsonschema:"required" jsonschema_description:"用户本回合明确要求忘记的长期记忆键"`
}

type TimelineCheckInput struct {
	TimelineID string `json:"timeline_id,omitempty" jsonschema_description:"可选；检查该草稿下这个稳定 timeline_id 指向的精确版本。省略时检查当前版本"`
}

type TimelineInspectInput struct {
	TimelineID string `json:"timeline_id,omitempty" jsonschema_description:"可选；读取该草稿下这个稳定 timeline_id 指向的精确版本。省略时读取当前版本"`
}

type PreviewGenerateInput struct {
	TimelineID  string `json:"timeline_id" jsonschema:"required" jsonschema_description:"CurrentTimelineView 返回的当前稳定 timeline_id；它精确指向一个版本，若已变化则返回 stale_target，不猜测新目标"`
	Orientation string `json:"orientation,omitempty" jsonschema_description:"预览画幅方向：auto、portrait 或 landscape；默认 auto"`
}

type PreviewCheckInput struct {
	PreviewID string `json:"preview_id" jsonschema:"required" jsonschema_description:"preview.generate 成功返回的 preview_id"`
	Check     string `json:"check" jsonschema:"required" jsonschema_description:"本次唯一检查项：decode、black、freeze、silence、loudness 或 visual"`
}

type PreviewInspectionResult struct {
	PreviewID          string                   `json:"preview_id"`
	Check              string                   `json:"check"`
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
