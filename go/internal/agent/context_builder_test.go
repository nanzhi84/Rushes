package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

func TestContextBuilderOnlyExposesLatestTimelineAndCompressedSemanticEdits(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_context_latest")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	document := timeline.Empty("draft_context_latest", 1)
	document.DurationFrames = 30
	document.Tracks[0].Clips = []timeline.Clip{{
		TimelineClipID: "clip_context", TrackID: "visual_base", AssetID: "asset_context",
		AssetKind: "video", Role: "a_roll", TimelineStartFrame: 0, TimelineEndFrame: 30,
		SourceStartFrame: 0, SourceEndFrame: 30, PlaybackRate: 1, GainDB: -3,
	}}
	document.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "bgm_context", TrackID: "bgm", AssetID: "asset_bgm_context",
		AssetKind: "audio", Role: "bgm", TimelineStartFrame: 0, TimelineEndFrame: 30,
		SourceStartFrame: 0, SourceEndFrame: 30, PlaybackRate: 1,
		Effects: []map[string]any{{
			"kind": "beat_grid", "bpm": 120.0,
			"beat_frames": []int{15, 30}, "strong_beat_frames": []int{15},
			"downbeat_frames": []int{15}, "analysis_method": "fixture",
			"waveform": map[string]any{
				"sample_interval_frames": 10,
				"samples":                []int{5, 70, 10},
				"encoding":               "rms_db_-60_0_to_0_100",
				"floor_db":               -60.0,
				"ceiling_db":             0.0,
			},
		}},
	}}
	first, err := service.persistTimeline(
		t.Context(), "draft_context_latest", document, "context_first", []map[string]any{{
			"kind": "adjust_gain", "timeline_clip_id": "clip_context", "gain_db": -3,
			"timeline_revision": 24,
			"nested":            map[string]any{"timeline_version": 1, "version": 1, "timeline_id": "old"},
		}},
	)
	if err != nil || first.Status != "succeeded" {
		t.Fatalf("first=%#v err=%v", first, err)
	}

	document.Version = 2
	document.TimelineID = "draft_context_latest:v2"
	document.Tracks[0].Clips[0].GainDB = -9
	manualContext := rushestools.WithTimelineMutationOrigin(t.Context(), "manual")
	second, err := service.persistTimeline(
		manualContext, "draft_context_latest", document, "context_second", []map[string]any{{
			"kind": "adjust_gain", "timeline_clip_id": "clip_context", "gain_db": -9,
			"timeline_version": 2, "draft_id": "draft_context_latest",
		}},
	)
	if err != nil || second.Status != "succeeded" {
		t.Fatalf("second=%#v err=%v", second, err)
	}

	contextText, err := NewContextBuilder(database).Build(t.Context(), "draft_context_latest")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		`"timeline_revision"`, `"timeline_version"`, `"timeline_id"`, `"draft_id"`, `"version"`,
	} {
		if strings.Contains(contextText, forbidden) {
			t.Fatalf("模型上下文仍包含废弃字段 %s: %s", forbidden, contextText)
		}
	}
	if !strings.Contains(contextText, `"gain_db":-9`) || strings.Contains(contextText, `"gain_db":-3`) {
		t.Fatalf("模型未只看到最新时间线: %s", contextText)
	}
	if !strings.Contains(contextText, `"samples":[5,70,10]`) ||
		!strings.Contains(contextText, `"sample_interval_frames":10`) ||
		!strings.Contains(contextText, `"sample_frames":[0,10,20]`) {
		t.Fatalf("压缩波形未进入模型的最新时间线上下文: %s", contextText)
	}
	if strings.Count(contextText, `"kind":"adjust_gain"`) != 1 {
		t.Fatalf("重复操作未压缩: %s", contextText)
	}
	if !strings.Contains(contextText, `"actor":"user"`) ||
		!strings.Contains(contextText, `"origin":"manual"`) {
		t.Fatalf("人工编辑来源丢失: %s", contextText)
	}
}

func TestContextBuilderInjectsPersistentCompactMaterialCatalog(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_context_summaries")
	for index := 0; index < 30; index++ {
		assetID := fmt.Sprintf("asset_summary_%02d", index)
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO assets(
				asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
				probe_json,ingest_status,understanding_status,usable
			) VALUES(?, 'reference', ?, 'video', 'local_path', ?, ?, 1,
				'{"duration_sec":10}', 'ready', 'ready', 1);
			INSERT INTO draft_asset_links(draft_id,asset_id,linked_at)
			VALUES('draft_context_summaries', ?, ?);`,
			assetID, "/tmp/"+assetID+".mp4", assetID+".mp4", assetID,
			assetID, fmt.Sprintf("2026-01-01T00:00:%02dZ", index),
		); err != nil {
			t.Fatal(err)
		}
		for version, prefix := range []string{"obsolete-", "latest-"} {
			summary, err := json.Marshal(map[string]any{
				"asset_id": assetID, "version": version + 1,
				"semantic_role":  "b_roll",
				"analysis_depth": "deep",
				"overall":        prefix + assetID + strings.Repeat("画面", 220),
				"segments": []map[string]any{{
					"start_s": 0, "end_s": 10,
					"description": prefix + assetID + strings.Repeat("动作", 160),
					"tags":        []string{"人物", "动作"}, "quality": "usable",
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.Write().ExecContext(t.Context(), `
				INSERT INTO material_summaries(
					summary_id,asset_id,version,status,summary_json,created_at
				) VALUES(?,?,?,'ready',?,?)`,
				fmt.Sprintf("summary_%02d_%d", index, version+1), assetID,
				version+1, string(summary), fmt.Sprintf("2026-01-02T00:00:%02dZ", version),
			); err != nil {
				t.Fatal(err)
			}
		}
	}

	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	document := timeline.Empty("draft_context_summaries", 1)
	document.DurationFrames = 30
	document.Tracks[0].Clips = []timeline.Clip{{
		TimelineClipID: "clip_prioritized_summary", TrackID: "visual_base",
		AssetID: "asset_summary_00", AssetKind: "video", Role: "b_roll",
		TimelineStartFrame: 0, TimelineEndFrame: 30,
		SourceStartFrame: 0, SourceEndFrame: 30, PlaybackRate: 1,
	}}
	if result, err := service.persistTimeline(
		t.Context(), "draft_context_summaries", document, "summary_context",
	); err != nil || result.Status != "succeeded" {
		t.Fatalf("persist=%#v err=%v", result, err)
	}

	contextText, err := NewContextBuilder(database).Build(t.Context(), "draft_context_summaries")
	if err != nil {
		t.Fatal(err)
	}
	prefix := "【当前草稿最新 WorldState】\n"
	jsonEnd := strings.Index(contextText, "\nsections 是当前客观状态的唯一事实源")
	if !strings.HasPrefix(contextText, prefix) || jsonEnd < 0 {
		t.Fatalf("上下文格式错误: %s", contextText)
	}
	var snapshot map[string]any
	if err := json.Unmarshal([]byte(contextText[len(prefix):jsonEnd]), &snapshot); err != nil {
		t.Fatal(err)
	}
	sections := snapshot["sections"].(map[string]any)
	assetContext := sections["assets"].(map[string]any)
	items := assetContext["material_catalog"].([]any)
	if len(items) == 0 || len(items) > 30 {
		t.Fatalf("material catalog=%d", len(items))
	}
	if assetContext["material_catalog_available"] != float64(30) ||
		assetContext["material_catalog_truncated"] != false {
		t.Fatalf("material catalog metadata=%#v", assetContext)
	}
	foundTimelineAsset := false
	for _, raw := range items {
		item := raw.(map[string]any)
		assetID := item["asset_id"].(string)
		if assetID == "asset_summary_00" {
			foundTimelineAsset = true
		}
		if !strings.HasPrefix(item["overall"].(string), "latest-"+assetID) {
			t.Fatalf("未读取最佳摘要: %#v", item)
		}
		if item["analysis_depth"] != "deep" {
			t.Fatalf("常驻目录丢失理解深度: %#v", item)
		}
		if len([]rune(item["overall"].(string))) > 161 {
			t.Fatalf("overall 未压缩: %#v", item)
		}
		if _, exists := item["evidence"]; exists {
			t.Fatalf("常驻目录不应携带逐镜头 evidence: %#v", item)
		}
	}
	if !foundTimelineAsset {
		t.Fatalf("时间线素材未进入常驻目录: %#v", items)
	}
	encodedItems, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(string(encodedItems))) > contextMaterialCatalogRuneBudget {
		t.Fatalf("素材目录超出字符预算: %d", len([]rune(string(encodedItems))))
	}
	if strings.Contains(contextText, "obsolete-") || strings.Contains(contextText, "generated_at") {
		t.Fatalf("上下文含历史或编排元数据: %s", contextText)
	}
}

func TestMaterialCatalogKeepsAudioRoleAndStopsAtBudget(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	items, available, err := NewContextBuilder(database).materialCatalogContext(t.Context(), []storage.Asset{
		{
			ID: "audio_catalog", Filename: "IGNIS BGM.wav", Kind: "audio",
			UnderstandingStatus: "none", Probe: map[string]any{"duration_sec": 48.0},
		},
		{
			ID: strings.Repeat("oversized-asset", 1000), Filename: strings.Repeat("x", 13000),
			Kind: "video", UnderstandingStatus: "none", Probe: map[string]any{"duration_sec": 60.0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if available != 2 || len(items) != 1 || items[0]["suggested_role"] != "bgm" {
		t.Fatalf("budget catalog=%#v available=%d", items, available)
	}
}

func TestCompactUnderstandingSummaryPreservesDirectFrameEvidenceAcrossAsset(t *testing.T) {
	t.Parallel()
	segments := make([]understanding.Segment, 0, 20)
	for index := 0; index < 20; index++ {
		score := float64(index) / 20
		segments = append(segments, understanding.Segment{
			StartSec: float64(index), EndSec: float64(index + 1),
			SourceStartFrame: 1000 + index*30, SourceEndFrame: 1030 + index*30,
			Description: fmt.Sprintf("镜头 %d", index), Tags: []string{"动作"},
			Quality: "usable", BoundaryKind: "visual_cut", BoundaryScore: &score,
			BoundaryVerified: true,
		})
	}
	compact := compactUnderstandingSummary(storage.Asset{
		ID: "asset_direct_frames", Filename: "direct.mp4", Kind: "video",
	}, understanding.Summary{
		Overall: "完整素材", SemanticRole: "b_roll", Segments: segments,
		AnalysisMethod: "ffmpeg_scdet_vlm_verify", CandidateCuts: 19, VerifiedCuts: 18,
	}, 4)
	if len(compact.Evidence) != 4 || compact.EvidenceTotal != 20 || !compact.EvidenceTruncated {
		t.Fatalf("evidence sampling=%#v", compact)
	}
	if compact.Evidence[0].SourceStartFrame != 1000 ||
		compact.Evidence[len(compact.Evidence)-1].SourceEndFrame != 1600 {
		t.Fatalf("未覆盖素材首尾或错误重算直接帧: %#v", compact.Evidence)
	}
	if compact.Evidence[1].BoundaryScore == nil || !compact.Evidence[1].BoundaryVerified ||
		compact.AnalysisMethod != "ffmpeg_scdet_vlm_verify" || compact.CandidateCutCount != 19 ||
		compact.VerifiedCutCount != 18 {
		t.Fatalf("检测证据元数据丢失: %#v", compact)
	}
	if !strings.Contains(compact.UsageNote, "analysis_window") ||
		!strings.Contains(compact.UsageNote, "boundary_verified=true") {
		t.Fatalf("usage note=%q", compact.UsageNote)
	}
}

func TestCompactUnderstandingSummaryNormalizesDerivedFramesAndRichSemantics(t *testing.T) {
	t.Parallel()
	transcript := strings.Repeat("台词", 100)
	compact := compactUnderstandingSummary(storage.Asset{
		ID: "asset_derived_frames", Filename: "derived.mp4", Kind: "video",
	}, understanding.Summary{
		Overall: "相同描述",
		Segments: []understanding.Segment{{
			StartSec: -1, EndSec: -0.5, SourceStartFrame: -1, SourceEndFrame: -1,
			Description: "相同描述", Transcript: &transcript,
			Tags:     []string{"一", "二", "三", "四", "五", "六", "七"},
			Subjects: []string{"人物"}, Actions: []string{"转身"}, Setting: []string{"海边"},
			ShotScale: "近景", Composition: "居中", Lighting: []string{"低调光"},
			Mood: []string{"紧张"}, EditHints: []string{"动作峰值切入"},
		}},
		Degraded: []string{"a", "b", "c", "d", "e"},
	}, 12)
	if len(compact.Evidence) != 1 || compact.Evidence[0].Description != "" ||
		compact.Evidence[0].SourceStartFrame != 0 || compact.Evidence[0].SourceEndFrame != 1 ||
		len([]rune(compact.Evidence[0].Transcript)) > 161 || len(compact.Evidence[0].Tags) != 6 ||
		compact.Evidence[0].ShotScale != "近景" || len(compact.Degraded) != 4 {
		t.Fatalf("derived compact=%#v", compact)
	}
}

func TestContextCompactionHandlesBeatGridAndMalformedStoredSummary(t *testing.T) {
	t.Parallel()
	if compactBeatGridContext(nil) != nil ||
		compactBeatGridContext([]map[string]any{{"kind": "other"}}) != nil {
		t.Fatal("非节拍效果不应进入上下文")
	}
	grid := compactBeatGridContext([]map[string]any{
		{"kind": "other"},
		{
			"kind": "beat_grid", "bpm": 120.0, "analysis_method": "test",
			"beat_frames": []int{10, 20}, "strong_beat_frames": []any{20.0},
			"downbeat_frames": []int{10},
			"waveform": map[string]any{
				"sample_interval_frames": 15.0,
				"samples":                []any{4.0, 72.0, 18.0},
				"encoding":               "rms_db_-60_0_to_0_100",
				"floor_db":               -60.0,
				"ceiling_db":             0.0,
			},
		},
	})
	if grid["beat_count"] != 2 || grid["strong_beat_count"] != 1 ||
		grid["downbeat_count"] != 1 || grid["analysis_method"] != "test" {
		t.Fatalf("grid=%#v", grid)
	}
	waveform, ok := grid["waveform"].(map[string]any)
	if !ok || waveform["sample_interval_frames"] != 15 ||
		!reflect.DeepEqual(waveform["sample_frames"], []int{0, 15, 30}) ||
		!reflect.DeepEqual(waveform["samples"], []int{4, 72, 18}) {
		t.Fatalf("waveform=%#v", waveform)
	}
	if compactWaveformContext(map[string]any{
		"sample_interval_frames": 15,
		"samples":                []int{0, 101},
	}) != nil {
		t.Fatal("超出固定编码范围的波形不应进入上下文")
	}
	if compactWaveformContext(map[string]any{
		"sample_interval_frames": 15,
		"sample_frames":          []int{0, 15},
		"samples":                []int{20},
	}) != nil {
		t.Fatal("坐标和值数量不一致的波形不应进入上下文")
	}
}

func TestCompressTimelineEditBatchesCancelsTransientInsertDelete(t *testing.T) {
	t.Parallel()
	batches := []struct {
		actor, origin string
		ops           []map[string]any
	}{
		{"user", "manual", []map[string]any{{
			"kind": "insert_clip", "timeline_clip_id": "temporary",
		}}},
		{"user", "manual", []map[string]any{{
			"kind": "delete_clip", "timeline_clip_id": "temporary",
		}}},
	}
	converted := makeTimelineEditBatches(batches)
	if compressed := compressTimelineEditBatches(converted, 20); len(compressed) != 0 {
		t.Fatalf("短暂插入后删除不应进入模型历史: %#v", compressed)
	}
}

func TestCompressTimelineEditBatchesSummarizesAndDeduplicatesWholeTimelineRecuts(t *testing.T) {
	t.Parallel()
	largeCuts := make([]int, 0, 2000)
	largeAssets := make([]string, 0, 2000)
	largeRanges := make([]map[string]any, 0, 2000)
	largeShots := make([]string, 0, 2000)
	for index := 0; index < 2000; index++ {
		largeCuts = append(largeCuts, (index+1)*15)
		largeAssets = append(largeAssets, fmt.Sprintf("asset_%d", index%24))
		largeRanges = append(largeRanges, map[string]any{
			"asset_id":           fmt.Sprintf("asset_%d", index%24),
			"source_start_frame": index * 30, "source_end_frame": index*30 + 15,
		})
		largeShots = append(largeShots, fmt.Sprintf("shot_%d", index))
	}
	makeRecut := func(bgm string, target int) map[string]any {
		return map[string]any{
			"kind": "recut_to_beats", "bgm_asset_id": bgm,
			"target_duration_frames": target, "video_asset_ids": largeAssets,
			"cut_frames": largeCuts, "source_range_usage": largeRanges,
			"shot_ids": largeShots, "sfx_asset_id": "sfx_fire", "sfx_start_frame": 1372,
		}
	}
	batches := makeTimelineEditBatches([]struct {
		actor, origin string
		ops           []map[string]any
	}{
		{"agent", "tool", []map[string]any{makeRecut("bgm_old", 1200)}},
		{"user", "manual", []map[string]any{{
			"kind": "adjust_gain", "timeline_clip_id": "old_clip", "gain_db": -8,
		}}},
		{"agent", "tool", []map[string]any{makeRecut("bgm_latest", 1440)}},
		{"user", "manual", []map[string]any{{
			"kind": "adjust_gain", "timeline_clip_id": "bgm_latest_clip", "gain_db": -10,
		}}},
	})

	compressed := compressTimelineEditBatches(batches, contextRecentEditLimit)
	if len(compressed) != 2 {
		t.Fatalf("compressed=%#v", compressed)
	}
	recut := compressed[0]["op"].(map[string]any)
	if recut["kind"] != "recut_to_beats" || recut["bgm_asset_id"] != "bgm_latest" ||
		recut["clip_count"] != 2000 || recut["video_asset_count"] != 24 ||
		recut["source_range_count"] != 2000 || recut["shot_count"] != 2000 ||
		recut["uses_explicit_shots"] != true || recut["first_cut_frame"] != 15 ||
		recut["last_cut_frame"] != 30000 {
		t.Fatalf("recut summary=%#v", recut)
	}
	encoded, err := json.Marshal(compressed)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"cut_frames", "video_asset_ids", "source_range_usage", "shot_ids", "bgm_old", "old_clip"} {
		if strings.Contains(text, `"`+forbidden+`"`) || strings.Contains(text, forbidden) {
			t.Fatalf("压缩历史仍包含冗余字段 %s: %s", forbidden, text)
		}
	}
	if len([]rune(text)) > contextRecentEditRuneBudget {
		t.Fatalf("recent_edit_history 超出预算: %d", len([]rune(text)))
	}
}

func TestCompressTimelineEditBatchesBoundsGenericLargeCollections(t *testing.T) {
	t.Parallel()
	operations := make([]map[string]any, 0, 30)
	for index := 0; index < 30; index++ {
		operations = append(operations, map[string]any{
			"kind": "custom_edit", "timeline_clip_id": fmt.Sprintf("clip_%02d", index),
			"notes": strings.Repeat("语义说明", 400), "frames": make([]int, 2000),
		})
	}
	compressed := compressTimelineEditBatches(makeTimelineEditBatches([]struct {
		actor, origin string
		ops           []map[string]any
	}{{"user", "manual", operations}}), contextRecentEditLimit)
	encoded, err := json.Marshal(compressed)
	if err != nil {
		t.Fatal(err)
	}
	if len(compressed) == 0 || len(compressed) > contextRecentEditLimit ||
		len([]rune(string(encoded))) > contextRecentEditRuneBudget ||
		strings.Contains(string(encoded), strings.Repeat("语义说明", 100)) {
		t.Fatalf("generic compressed=%s", encoded)
	}
}

func TestRecentEditHistoryBudgetFallsBackToMinimalLatestEntry(t *testing.T) {
	t.Parallel()
	entry := map[string]any{
		"actor": "user", "origin": "manual",
		"op": map[string]any{
			"kind": "custom_oversized", "timeline_clip_id": strings.Repeat("clip", 1000),
			"payload": strings.Repeat("内容", 10000),
		},
	}
	bounded := boundRecentEditHistory([]map[string]any{entry}, 400)
	if len(bounded) != 1 {
		t.Fatalf("bounded=%#v", bounded)
	}
	operation := bounded[0]["op"].(map[string]any)
	if operation["kind"] != "custom_oversized" || len([]rune(operation["target"].(string))) > 121 {
		t.Fatalf("minimal=%#v", operation)
	}
	if boundRecentEditHistory([]map[string]any{entry}, 0) != nil ||
		boundRecentEditHistory(nil, contextRecentEditRuneBudget) != nil {
		t.Fatal("空历史或零预算必须返回 nil")
	}
	unencodable := map[string]any{"op": map[string]any{"kind": make(chan int)}}
	if boundRecentEditHistory([]map[string]any{unencodable}, 20) != nil {
		t.Fatal("不可编码且无法最小化的历史必须舍弃")
	}
}

func TestContextCompressionHelpersBoundAndSanitizeHistory(t *testing.T) {
	t.Parallel()
	longGoal := strings.Repeat("剪", 220)
	if truncateRunes("短文本", 10) != "短文本" || !strings.HasSuffix(truncateRunes(longGoal, 10), "…") {
		t.Fatal("rune 截断行为错误")
	}
	semanticTags := catalogSemanticTags([]understanding.Segment{{
		Tags: []string{"", "人物", "人物"}, Subjects: []string{"舞者"}, Actions: []string{"转身"},
	}}, 3)
	if !reflect.DeepEqual(semanticTags, []string{"人物", "舞者", "转身"}) {
		t.Fatalf("semantic tags=%v", semanticTags)
	}

	sanitized := sanitizeContextValue([]any{
		map[string]any{"version": 3, "keep": []map[string]any{{"timeline_id": "old", "value": 1}}},
	}).([]any)
	nested := sanitized[0].(map[string]any)
	if _, exists := nested["version"]; exists || nested["keep"].([]map[string]any)[0]["value"] != 1 {
		t.Fatalf("nested sanitize=%#v", sanitized)
	}
	if valueOrContext("  ", "fallback") != "fallback" || valueOrContext("goal", "fallback") != "goal" {
		t.Fatal("context fallback 错误")
	}
	for _, value := range []any{"", 0, float64(0), false, nil} {
		if !emptyContextValue(value) {
			t.Fatalf("应识别为空值: %#v", value)
		}
	}
	if emptyContextValue(1) || emptyContextValue(true) || emptyContextValue(struct{}{}) {
		t.Fatal("非空值被误判")
	}
}

func TestEditHistoryIndexKeysCoverAllCoalescibleOperations(t *testing.T) {
	t.Parallel()
	entries := []map[string]any{
		{"op": map[string]any{"kind": "insert_clip", "timeline_clip_id": "clip_a"}},
		{"op": map[string]any{"kind": "move_clip", "clip_id": "clip_a"}},
		{"op": map[string]any{"kind": "trim_clip_edge", "timeline_clip_id": "clip_a", "edge": "end"}},
		{"op": map[string]any{"kind": "set_track_state", "track_id": "bgm"}},
	}
	coalesced, inserted := rebuildEditIndexes(entries)
	if inserted["clip_a"] != 0 || coalesced["move_clip:clip_a"] != 1 ||
		coalesced["trim_clip_edge:clip_a:end"] != 2 || coalesced["set_track_state:bgm"] != 3 {
		t.Fatalf("coalesced=%#v inserted=%#v", coalesced, inserted)
	}
	for _, kind := range []string{
		"adjust_gain", "set_clip_fades", "set_clip_linked", "edit_subtitle_text", "set_playback_rate",
	} {
		if coalesceOperationKey(kind, map[string]any{}, "clip") != kind+":clip" {
			t.Fatalf("missing coalesce key for %s", kind)
		}
	}
	if coalesceOperationKey("recut_to_beats", map[string]any{}, "") != "recut_to_beats" ||
		coalesceOperationKey("compose_initial", map[string]any{}, "") != "compose_initial" {
		t.Fatal("整时间线替换操作必须折叠为单条最新摘要")
	}
	if coalesceOperationKey("delete_clip", map[string]any{}, "clip") != "" ||
		operationTarget(map[string]any{"unknown": true}) != "" {
		t.Fatal("结构性操作不应被折叠")
	}
}

func makeTimelineEditBatches(values []struct {
	actor, origin string
	ops           []map[string]any
}) []storage.TimelineEditBatch {
	result := make([]storage.TimelineEditBatch, 0, len(values))
	for index, value := range values {
		result = append(result, storage.TimelineEditBatch{
			ID: fmt.Sprintf("batch_%d", index), Actor: value.actor,
			Origin: value.origin, Operations: value.ops,
		})
	}
	return result
}
