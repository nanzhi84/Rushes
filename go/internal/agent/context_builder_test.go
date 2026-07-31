package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

func TestCurrentTimelineViewExposesLatestTimelineAndOrderedSemanticEdits(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_context_latest")
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
	first, err := seedTimelineVersion(service,
		t.Context(), "draft_context_latest", document, "context_first", map[string]any{
			"kind": "adjust_gain", "timeline_clip_id": "clip_context", "gain_db": -3,
			"timeline_revision": 24,
			"nested":            map[string]any{"timeline_version": 1, "version": 1, "timeline_id": "old"},
		},
	)
	if err != nil || first.Status != "succeeded" {
		t.Fatalf("first=%#v err=%v", first, err)
	}

	document.Version = 2
	document.TimelineID = "draft_context_latest:v2"
	document.Tracks[0].Clips[0].GainDB = -9
	manualContext := rushestools.WithTimelineMutationOrigin(t.Context(), "manual")
	second, err := seedTimelineVersion(service,
		manualContext, "draft_context_latest", document, "context_second", map[string]any{
			"kind": "adjust_gain", "timeline_clip_id": "clip_context", "gain_db": -9,
			"timeline_version": 2, "draft_id": "draft_context_latest",
		},
	)
	if err != nil || second.Status != "succeeded" {
		t.Fatalf("second=%#v err=%v", second, err)
	}

	view, err := buildCurrentTimelineView(t.Context(), database, "draft_context_latest")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	contextText := string(raw)
	if view["draft_id"] != "draft_context_latest" ||
		view["timeline_id"] != "draft_context_latest:v2" || view["version"] != 2 {
		t.Fatalf("CurrentTimelineView identity=%#v", view)
	}
	clips := view["clips"].([]map[string]any)
	if len(clips) != 2 || clips[0]["gain_db"] != float64(-9) {
		t.Fatalf("模型未看到最新时间线 clip: %#v", clips)
	}
	if !strings.Contains(contextText, `"samples":[5,70,10]`) ||
		!strings.Contains(contextText, `"sample_interval_frames":10`) ||
		!strings.Contains(contextText, `"sample_frames":[0,10,20]`) {
		t.Fatalf("压缩波形未进入模型的最新时间线上下文: %s", contextText)
	}
	if strings.Count(contextText, `"kind":"adjust_gain"`) != 2 {
		t.Fatalf("有序编辑历史丢失了同目标操作: %s", contextText)
	}
	if !strings.Contains(contextText, `"actor":"agent"`) ||
		!strings.Contains(contextText, `"actor":"user"`) ||
		!strings.Contains(contextText, `"origin":"manual"`) {
		t.Fatalf("编辑来源或顺序丢失: %s", contextText)
	}
}

func TestContextBuilderInjectsPersistentCompactMaterialCatalog(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_context_summaries")
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
	if result, err := seedTimelineVersion(service,
		t.Context(), "draft_context_summaries", document, "summary_context", nil); err != nil || result.Status != "succeeded" {
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
	database := agenttest.AgentTestDatabase(t)
	items, available, err := NewContextBuilder(database).materialCatalogContext(t.Context(), []storage.Asset{
		{
			ID: "audio_catalog", Filename: "IGNIS BGM.wav", Kind: "audio",
			UnderstandingStatus: "none", Probe: map[string]any{"duration_sec": 48.0},
		},
		{
			ID: strings.Repeat("oversized-asset", 1000), Filename: strings.Repeat("x", 13000),
			Kind: "video", UnderstandingStatus: "none", Probe: map[string]any{"duration_sec": 60.0},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if available != 2 || len(items) != 1 || items[0]["suggested_role"] != "bgm" {
		t.Fatalf("budget catalog=%#v available=%d", items, available)
	}
}

func TestContextBuilderShowsPlanHintAndDeduplicatesAudioRoles(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_plan_hint")
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES('audio_plan_hint','reference','/tmp/plan-bgm.wav','audio','local_path',
			'plan-bgm.wav','audio_plan_hint',1,'{"duration_sec":30}','ready','none',1);
		INSERT INTO draft_asset_links(draft_id,asset_id,linked_at)
		VALUES('draft_plan_hint','audio_plan_hint','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewContextBuilder(database).Snapshot(t.Context(), "draft_plan_hint")
	if err != nil {
		t.Fatal(err)
	}
	draft := snapshot.Sections["draft"].(map[string]any)
	plan := draft["content_plan"].(map[string]any)
	if plan["_hint"] != "尚未建立创作计划" {
		t.Fatalf("draft=%#v", draft)
	}
	audioRoles := agentexec.WorldStateObjectSlice(snapshot.Sections["assets"].(map[string]any)["audio_roles"])
	if len(audioRoles) != 1 || len(audioRoles[0]) != 2 ||
		audioRoles[0]["asset_id"] != "audio_plan_hint" || audioRoles[0]["suggested_role"] != "bgm" {
		t.Fatalf("audio_roles=%#v", audioRoles)
	}
}

func TestMaterialCatalogPrioritizesUsedAndTranscriptAssetsWithStableOutput(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const transcriptID = "asset_catalog_119"
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES(?, 'reference', '/tmp/transcript.mp4', 'video', 'local_path',
			'transcript.mp4', ?, 1, '{"duration_sec":10}', 'ready', 'none', 1);
		INSERT INTO transcripts(
			transcript_id,asset_id,provider_id,raw_preserved,utterances_json,vad_segments_json
		) VALUES('transcript_catalog_119', ?, 'fixture', 0,
			'[{"utterance_id":"utt_119","text":"关键台词"}]', '[]')`,
		transcriptID, transcriptID, transcriptID,
	); err != nil {
		t.Fatal(err)
	}
	assets := make([]storage.Asset, 0, 120)
	for index := 0; index < 120; index++ {
		assets = append(assets, storage.Asset{
			ID:       fmt.Sprintf("asset_catalog_%03d", index),
			Filename: strings.Repeat(fmt.Sprintf("素材%03d", index), 20) + ".mp4",
			Kind:     "video", UnderstandingStatus: "none",
			Probe: map[string]any{"duration_sec": 10.0},
		})
	}
	used := map[string]struct{}{"asset_catalog_118": {}}
	builder := NewContextBuilder(database)
	first, available, err := builder.materialCatalogContext(t.Context(), assets, used)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := builder.materialCatalogContext(t.Context(), assets, used)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if available != 120 || string(firstJSON) != string(secondJSON) {
		t.Fatalf("available=%d stable=%v", available, string(firstJSON) == string(secondJSON))
	}
	foundUsed, foundTranscript := false, false
	lastID := ""
	for _, item := range first {
		id := item["asset_id"].(string)
		if lastID != "" && id < lastID {
			t.Fatalf("catalog output lost linked order: %s before %s", lastID, id)
		}
		lastID = id
		foundUsed = foundUsed || id == "asset_catalog_118"
		foundTranscript = foundTranscript || id == transcriptID && item["transcript_provider"] == "fixture"
	}
	if !foundUsed || !foundTranscript || len(first) >= available {
		t.Fatalf("used=%v transcript=%v included=%d available=%d", foundUsed, foundTranscript, len(first), available)
	}
}

func TestRecentEditHistoryIsStableOrderedArray(t *testing.T) {
	t.Parallel()
	batches := []storage.TimelineEditBatch{
		{Sequence: 10, Actor: "agent", Origin: "tool", Operations: []map[string]any{{"kind": "adjust_gain", "timeline_clip_id": "clip_a", "gain_db": -3}}},
		{Sequence: 11, Actor: "user", Origin: "manual", Operations: []map[string]any{{"kind": "move_clip", "timeline_clip_id": "clip_b", "target_frame": 30}}},
	}
	baseHistory := compressTimelineEditHistoryMap(batches[:1], contextRecentEditLimit)
	currentHistory := compressTimelineEditHistoryMap(batches, contextRecentEditLimit)
	base := NewWorldStateSnapshot(map[string]any{"recent_edit_history": baseHistory})
	current := NewWorldStateSnapshot(map[string]any{"recent_edit_history": currentHistory})
	patch, err := base.MergePatchTo(current)
	if err != nil {
		t.Fatal(err)
	}
	historyPatch := patch["sections"].(map[string]any)["recent_edit_history"].([]any)
	if len(historyPatch) != 2 {
		t.Fatalf("ordered history replacement=%#v", historyPatch)
	}
	first := historyPatch[0].(map[string]any)
	second := historyPatch[1].(map[string]any)
	if first["sequence"] != float64(10) || second["sequence"] != float64(11) ||
		first["actor"] != "agent" || second["actor"] != "user" {
		t.Fatalf("ordered history lost provenance: %#v", historyPatch)
	}

	evicted := NewWorldStateSnapshot(map[string]any{"recent_edit_history": []any{}})
	deletePatch, err := current.MergePatchTo(evicted)
	if err != nil {
		t.Fatal(err)
	}
	deleted := deletePatch["sections"].(map[string]any)["recent_edit_history"].([]any)
	if len(deleted) != 0 {
		t.Fatalf("history eviction patch=%#v", deleted)
	}
}

func TestRecentEditHistoryKeepsNewestOperationsAcrossThousandBoundary(t *testing.T) {
	t.Parallel()
	operations := make([]map[string]any, 0, 1010)
	for index := range 1010 {
		operations = append(operations, map[string]any{
			"kind":             "custom_edit",
			"timeline_clip_id": fmt.Sprintf("clip_%04d_%s", index, strings.Repeat("x", 180)),
			"detail_a":         strings.Repeat("甲", 500),
			"detail_b":         strings.Repeat("乙", 500),
		})
	}
	history := compressTimelineEditHistoryMap([]storage.TimelineEditBatch{{
		Sequence: 42, Actor: "agent", Origin: "tool", Operations: operations,
	}}, contextRecentEditLimit)
	if len(history) == 0 || len(history) >= contextRecentEditLimit {
		t.Fatalf("fixture must trigger rune eviction: retained=%d", len(history))
	}
	latest := history[len(history)-1]
	if latest["operation_index"] != 1009 {
		t.Fatalf("latest operation was evicted across 999 boundary: entries=%d", len(history))
	}
	for _, entry := range history {
		if index, _ := entry["operation_index"].(int); index < 990 {
			t.Fatalf("older operation %d survived before newer entries", index)
		}
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
	compact := agentexec.CompactUnderstandingSummary(storage.Asset{
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
	compact := agentexec.CompactUnderstandingSummary(storage.Asset{
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
		waveform["point_count"] != 3 || waveform["loudness_min"] != 4 ||
		waveform["loudness_mean"] != 31.3 || waveform["loudness_max"] != 72 ||
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

func TestCompactSemanticAnchorContextKeepsOnlyBoundedModelFields(t *testing.T) {
	t.Parallel()
	if compactSemanticAnchorContext(nil) != nil ||
		compactSemanticAnchorContext(map[string]any{"kind": "other"}) != nil {
		t.Fatal("非语义锚点元数据不应进入模型上下文")
	}
	anchor := compactSemanticAnchorContext(map[string]any{
		"kind":                      "b_roll_semantic_anchor",
		"shot_id":                   "shot_1",
		"a_roll_asset_id":           "asset_a",
		"a_roll_source_start_frame": 10,
		"a_roll_source_end_frame":   40,
		"start_utterance_id":        "utterance_1",
		"end_utterance_id":          "utterance_2",
		"start_word_id":             "word_1",
		"end_word_id":               "word_2",
		"b_roll_asset_id":           "asset_b",
		"b_roll_filename":           "b.mp4",
		"transcript_text":           "  " + strings.Repeat("台词", 100) + "  ",
		"b_roll_description":        strings.Repeat("画面", 100),
		"empty_field":               "",
		"internal_score":            0.97,
	})
	if anchor["kind"] != "b_roll_semantic_anchor" ||
		anchor["shot_id"] != "shot_1" ||
		anchor["a_roll_source_start_frame"] != 10 ||
		anchor["b_roll_filename"] != "b.mp4" {
		t.Fatalf("anchor=%#v", anchor)
	}
	if len([]rune(anchor["transcript_text"].(string))) > 161 ||
		len([]rune(anchor["b_roll_description"].(string))) > 161 {
		t.Fatalf("语义锚点文本未压缩: %#v", anchor)
	}
	if _, exists := anchor["internal_score"]; exists {
		t.Fatalf("内部元数据不应进入模型上下文: %#v", anchor)
	}
}

func TestCompactWaveformContextKeepsBoundedResidentEnvelope(t *testing.T) {
	t.Parallel()
	frames := make([]int, 200)
	samples := make([]int, 200)
	for index := range frames {
		frames[index] = index * 15
		samples[index] = 50
	}
	samples[1] = 0
	samples[198] = 100
	waveform := compactWaveformContext(map[string]any{
		"sample_interval_frames": 15,
		"sample_frames":          frames,
		"samples":                samples,
		"encoding":               "rms_db_-60_0_to_0_100",
		"floor_db":               -60,
		"ceiling_db":             0,
	})
	if waveform == nil || waveform["point_count"] != 200 ||
		waveform["loudness_min"] != 0 || waveform["loudness_mean"] != 50.0 ||
		waveform["loudness_max"] != 100 {
		t.Fatalf("waveform=%#v", waveform)
	}
	residentFrames, framesOK := waveform["sample_frames"].([]int)
	residentSamples, samplesOK := waveform["samples"].([]int)
	if !framesOK || !samplesOK || len(residentFrames) != contextResidentWaveformPoints ||
		len(residentSamples) != contextResidentWaveformPoints ||
		reflect.DeepEqual(residentFrames, frames) || reflect.DeepEqual(residentSamples, samples) {
		t.Fatalf("resident frames=%#v samples=%#v", residentFrames, residentSamples)
	}
	for index, frame := range residentFrames {
		if frame < 0 || index > 0 && frame <= residentFrames[index-1] ||
			residentSamples[index] < 0 || residentSamples[index] > 100 {
			t.Fatalf("invalid resident point %d: frame=%d sample=%d", index, frame, residentSamples[index])
		}
	}
	if _, exists := waveform["encoding"]; exists {
		t.Fatalf("resident summary must omit full-wave metadata: %#v", waveform)
	}
	raw, err := json.Marshal(waveform)
	if err != nil {
		t.Fatal(err)
	}
	if runes := len([]rune(string(raw))); runes >= 350 {
		t.Fatalf("resident waveform too large: runes=%d json=%s", runes, raw)
	}
}

func TestCompactWaveformContextRejectsMalformedCoordinatesAndBoundsStoredPoints(t *testing.T) {
	t.Parallel()
	for name, value := range map[string]any{
		"not_object": "waveform",
		"zero_interval": map[string]any{
			"sample_interval_frames": 0,
			"samples":                []int{1},
		},
		"fractional_interval": map[string]any{
			"sample_interval_frames": 1.5,
			"samples":                []int{1},
		},
		"no_samples": map[string]any{
			"sample_interval_frames": 15,
		},
		"negative_frame": map[string]any{
			"sample_interval_frames": 15,
			"sample_frames":          []int{-1},
			"samples":                []int{1},
		},
		"non_increasing_frames": map[string]any{
			"sample_interval_frames": 15,
			"sample_frames":          []int{0, 0},
			"samples":                []int{1, 2},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if compact := compactWaveformContext(value); compact != nil {
				t.Fatalf("malformed waveform entered context: %#v", compact)
			}
		})
	}

	samples := make([]int, 300)
	for index := range samples {
		samples[index] = index % 101
	}
	compact := compactWaveformContext(map[string]any{
		"sample_interval_frames": 15,
		"samples":                samples,
	})
	if compact == nil || compact["point_count"] != 256 {
		t.Fatalf("stored waveform must be capped before resident compaction: %#v", compact)
	}
}

func TestCompressTimelineEditHistoryRetainsTransientInsertDeleteInOrder(t *testing.T) {
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
	if compressed := compressTimelineEditHistoryMap(converted, 20); len(compressed) != 2 {
		t.Fatalf("有序审计历史必须保留插入与删除: %#v", compressed)
	}
}

func TestCompressTimelineEditHistoryBoundsGenericLargeCollections(t *testing.T) {
	t.Parallel()
	operations := make([]map[string]any, 0, 30)
	for index := 0; index < 30; index++ {
		operations = append(operations, map[string]any{
			"kind": "custom_edit", "timeline_clip_id": fmt.Sprintf("clip_%02d", index),
			"notes": strings.Repeat("语义说明", 400), "frames": make([]int, 2000),
		})
	}
	compressed := compressTimelineEditHistoryMap(makeTimelineEditBatches([]struct {
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
		"operation": map[string]any{
			"kind": "custom_oversized", "timeline_clip_id": strings.Repeat("clip", 1000),
			"payload": strings.Repeat("内容", 10000),
		},
	}
	bounded := boundRecentEditHistory([]map[string]any{entry}, 400)
	if len(bounded) != 1 {
		t.Fatalf("bounded=%#v", bounded)
	}
	operation := bounded[0]["operation"].(map[string]any)
	if operation["kind"] != "custom_oversized" || len([]rune(operation["target"].(string))) > 121 {
		t.Fatalf("minimal=%#v", operation)
	}
	if boundRecentEditHistory([]map[string]any{entry}, 0) != nil ||
		boundRecentEditHistory(nil, contextRecentEditRuneBudget) != nil {
		t.Fatal("空历史或零预算必须返回 nil")
	}
	unencodable := map[string]any{"operation": map[string]any{"kind": make(chan int)}}
	if boundRecentEditHistory([]map[string]any{unencodable}, 20) != nil {
		t.Fatal("不可编码且无法最小化的历史必须舍弃")
	}
}

func TestContextCompressionHelpersBoundAndSanitizeHistory(t *testing.T) {
	t.Parallel()
	longGoal := strings.Repeat("剪", 220)
	if agentexec.TruncateRunes("短文本", 10) != "短文本" || !strings.HasSuffix(agentexec.TruncateRunes(longGoal, 10), "…") {
		t.Fatal("rune 截断行为错误")
	}
	semanticTags := agentexec.CatalogSemanticTags([]understanding.Segment{{
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
	plan := sanitizeContentPlanForContext(map[string]any{
		"version":  3,
		"section":  map[string]any{"version": 1},
		"contract": map[string]any{"custom": map[string]any{"timeline_id": "stale"}, "keep": true},
	}).(map[string]any)
	if _, exists := plan["version"]; exists || plan["section"].(map[string]any)["version"] != 1 {
		t.Fatalf("content plan top-level boundary mismatch: %#v", plan)
	}
	contract := plan["contract"].(map[string]any)
	if _, exists := contract["custom"].(map[string]any)["timeline_id"]; exists || contract["keep"] != true {
		t.Fatalf("content plan contract boundary mismatch: %#v", plan)
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

	deep := compactEditHistoryValue(map[string]any{"payload": "x"}, 5).(map[string]any)
	if deep["compacted"] != true {
		t.Fatalf("deep compact=%#v", deep)
	}
	wide := map[string]any{"kind": "custom"}
	for index := 0; index < 30; index++ {
		wide[fmt.Sprintf("field_%02d", index)] = index
	}
	wideCompact := compactEditHistoryValue(wide, 0).(map[string]any)
	if wideCompact["kind"] != "custom" || wideCompact["omitted_field_count"] != 7 {
		t.Fatalf("wide compact=%#v", wideCompact)
	}
	shortCollection := compactEditHistoryValue([]any{
		"first",
		map[string]any{"nested": "second"},
	}, 0).([]any)
	if len(shortCollection) != 2 || shortCollection[0] != "first" {
		t.Fatalf("short collection=%#v", shortCollection)
	}
	if minInt(2, 1) != 1 {
		t.Fatal("collection helper defensive branches mismatch")
	}
	minimal := minimalEditHistoryEntry(map[string]any{
		"sequence": int64(1), "operation_index": 1, "actor": "agent", "origin": "tool",
		"operation": map[string]any{"kind": "delete_clip", "timeline_clip_id": "clip_1"},
	})
	if minimal["sequence"] != int64(1) || minimal["operation_index"] != 1 {
		t.Fatalf("minimal=%#v", minimal)
	}
}

func TestEditHistoryIndexKeysCoverAllCoalescibleOperations(t *testing.T) {
	t.Parallel()
	entries := []map[string]any{
		{"op": map[string]any{"kind": "insert_clip", "timeline_clip_id": "clip_a"}},
		{"op": map[string]any{"kind": "move_clip", "timeline_clip_id": "clip_a"}},
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
