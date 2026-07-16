package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

func TestShotSearchFiltersSemanticsAndCurrentTimelineUsage(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_shot_search")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, fixture := range []struct {
		assetID  string
		filename string
		relDir   string
	}{
		{assetID: "video_search", filename: "video_search.mp4", relDir: "Broll"},
		{assetID: "video_missing", filename: "键盘指纹聚焦.mov", relDir: "Broll/键盘"},
	} {
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO assets(
				asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
				probe_json,ingest_status,understanding_status,usable
			) VALUES(?, 'reference', ?, 'video', 'local_path', ?, ?, 1,
				'{"duration_sec":4}', 'ready', ?, 1);
			INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
			VALUES('draft_shot_search', ?, ?, ?);`,
			fixture.assetID, "/tmp/"+fixture.filename, fixture.filename, fixture.assetID,
			map[string]string{"video_search": "ready", "video_missing": "none"}[fixture.assetID],
			fixture.assetID, fixture.relDir, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	summary, _ := json.Marshal(map[string]any{
		"asset_id": "video_search", "overall": "夜晚火焰与海边环境",
		"analysis_depth": "deep", "semantic_role": "b_roll",
		"segments": []map[string]any{
			{
				"source_start_frame": 0, "source_end_frame": 60,
				"description": "夜晚人物举起火把，适合高潮强拍切入",
				"tags":        []string{"火焰", "夜景"}, "subjects": []string{"舞者"},
				"actions": []string{"举起火把"}, "setting": []string{"夜晚海滩"},
				"mood": []string{"高能"}, "edit_hints": []string{"高潮强拍切入"},
				"quality": "usable", "boundary_kind": "visual_cut", "boundary_verified": true,
			},
			{
				"source_start_frame": 60, "source_end_frame": 120,
				"description": "白天海浪远景，适合作为环境建立镜头",
				"tags":        []string{"海浪", "远景"}, "quality": "usable",
			},
		},
	})
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO material_summaries(
			summary_id,asset_id,version,status,summary_json,fingerprint,prompt_version,created_at
		) VALUES('summary_search','video_search',1,'ready',?,'search-fingerprint','v3',?)`,
		string(summary), now,
	); err != nil {
		t.Fatal(err)
	}

	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), "draft_shot_search")
	if _, err := service.ExecuteTool(ctx, "media.search_shots", rushestools.ShotSearchInput{
		MinDurationFrames: 90, MaxDurationFrames: 30,
	}); err == nil {
		t.Fatal("无效时长范围应失败")
	}
	if _, err := service.ExecuteTool(ctx, "media.search_shots", rushestools.ShotSearchInput{
		AssetIDs: []string{"missing"},
	}); err == nil {
		t.Fatal("未知素材过滤应失败")
	}
	if _, err := service.ExecuteTool(ctx, "media.search_shots", rushestools.ShotSearchInput{
		SemanticRoles: []string{"supporting"},
	}); err == nil {
		t.Fatal("未知视觉角色应失败")
	}

	output, err := service.ExecuteTool(ctx, "media.search_shots", rushestools.ShotSearchInput{
		Query: "夜晚火焰人物 高潮", Tags: []string{"火焰"},
		MinDurationFrames: 30, MaxDurationFrames: 90, SemanticRoles: []string{"b_roll"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := output.(rushestools.ShotSearchResult)
	if len(result.Shots) != 1 || result.Shots[0].SourceStartFrame != 0 ||
		result.Shots[0].ShotID == "" || result.Shots[0].SemanticRole != "b_roll" ||
		len(result.MissingUnderstandingAssetIDs) != 1 ||
		len(result.Shots[0].MatchedQueryTerms) == 0 || len(result.Shots[0].MatchEvidence) == 0 {
		t.Fatalf("search result=%#v", result)
	}

	missingOutput, err := service.ExecuteTool(ctx, "media.search_shots", rushestools.ShotSearchInput{
		Query: "键盘指纹解锁", SemanticRoles: []string{"b_roll"}, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	missing := missingOutput.(rushestools.ShotSearchResult)
	if len(missing.Shots) != 0 || len(missing.UnderstandingCandidates) != 1 ||
		missing.UnderstandingCandidates[0].AssetID != "video_missing" ||
		missing.UnderstandingCandidates[0].Filename != "键盘指纹聚焦.mov" ||
		len(missing.UnderstandingCandidates[0].MatchedQueryTerms) == 0 ||
		len(missing.UnderstandingCandidates[0].MatchEvidence) == 0 {
		t.Fatalf("missing understanding search=%#v", missing)
	}

	allOutput, err := service.ExecuteTool(ctx, "media.search_shots", rushestools.ShotSearchInput{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	all := allOutput.(rushestools.ShotSearchResult)
	if len(all.Shots) != 1 || all.TotalMatches != 2 || !all.Truncated {
		t.Fatalf("limited search=%#v", all)
	}

	document, err := timeline.ComposeInitial("draft_shot_search", 1, []timeline.Selection{{
		AssetID: "video_search", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 60,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if persisted, err := service.persistTimeline(t.Context(), "draft_shot_search", document, "shot_search_fixture"); err != nil || persisted.Status != "succeeded" {
		t.Fatalf("persisted=%#v err=%v", persisted, err)
	}
	excludedOutput, err := service.ExecuteTool(ctx, "media.search_shots", rushestools.ShotSearchInput{
		Query: "火焰", ExcludeUsed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if excluded := excludedOutput.(rushestools.ShotSearchResult); len(excluded.Shots) != 0 {
		t.Fatalf("已用源区间未排除: %#v", excluded)
	}
}

func TestShotSearchJoinsTranscriptByOverlapAndMarksTermSource(t *testing.T) {
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_transcript_search")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,size,probe_json,ingest_status,understanding_status,usable)
		VALUES('video_transcript','reference','/tmp/transcript.mp4','video','local_path','transcript.mp4','transcript',1,'{"duration_sec":4}','ready','ready',1);
		INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at) VALUES('draft_transcript_search','video_transcript','Aroll',?);
		INSERT INTO transcripts(transcript_id,asset_id,provider_id,raw_preserved,utterances_json,vad_segments_json)
		VALUES('transcript_search','video_transcript','fixture',0,
			'[{"utterance_id":"u_cross","source_start_frame":50,"source_end_frame":70,"text":"跨段口令"},{"utterance_id":"u_second","source_start_frame":80,"source_end_frame":100,"text":"第二专属词"}]','[]')
	`, now); err != nil {
		t.Fatal(err)
	}
	badExposure, badSharpness := 0.95, 0.0
	normalExposure, normalSharpness := 0.01, 500.0
	summary, _ := json.Marshal(map[string]any{
		"asset_id": "video_transcript", "semantic_role": "a_roll",
		"segments": []map[string]any{
			{"source_start_frame": 0, "source_end_frame": 60, "description": "人物口播", "quality": "usable", "overexposed_ratio": badExposure, "sharpness_score": badSharpness},
			{"source_start_frame": 60, "source_end_frame": 120, "description": "人物口播", "quality": "usable", "overexposed_ratio": normalExposure, "sharpness_score": normalSharpness},
		},
	})
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO material_summaries(summary_id,asset_id,version,status,summary_json,fingerprint,prompt_version,created_at)
		VALUES('summary_transcript','video_transcript',1,'ready',?,'fp','v4',?)`, string(summary), now); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), "draft_transcript_search")
	crossRaw, err := service.ExecuteTool(ctx, "media.search_shots", rushestools.ShotSearchInput{Query: "跨段口令"})
	if err != nil {
		t.Fatal(err)
	}
	cross := crossRaw.(rushestools.ShotSearchResult)
	if len(cross.Shots) != 2 {
		t.Fatalf("cross shots=%#v", cross.Shots)
	}
	for _, shot := range cross.Shots {
		hasTranscriptTerm, hasTranscriptEvidence := false, false
		for _, term := range shot.MatchedQueryTerms {
			hasTranscriptTerm = hasTranscriptTerm || strings.HasPrefix(term, "台词:")
		}
		for _, evidence := range shot.MatchEvidence {
			hasTranscriptEvidence = hasTranscriptEvidence || strings.HasPrefix(evidence, "台词命中:")
		}
		if !strings.Contains(shot.Transcript, "跨段口令") || !hasTranscriptTerm || !hasTranscriptEvidence {
			t.Fatalf("shot=%#v", shot)
		}
	}
	secondRaw, err := service.ExecuteTool(ctx, "media.search_shots", rushestools.ShotSearchInput{Query: "第二专属词"})
	if err != nil {
		t.Fatal(err)
	}
	second := secondRaw.(rushestools.ShotSearchResult)
	if len(second.Shots) != 1 || second.Shots[0].SourceStartFrame != 60 || !strings.Contains(second.Shots[0].Transcript, "第二专属词") {
		t.Fatalf("second shots=%#v", second.Shots)
	}
	qualityRaw, err := service.ExecuteTool(ctx, "media.search_shots", rushestools.ShotSearchInput{Query: "人物口播"})
	if err != nil {
		t.Fatal(err)
	}
	quality := qualityRaw.(rushestools.ShotSearchResult)
	if len(quality.Shots) != 2 || quality.Shots[0].SourceStartFrame != 60 || quality.Shots[1].SourceStartFrame != 0 {
		t.Fatalf("quality ranking=%#v", quality.Shots)
	}
}

func TestShotQualityMetricsGentlyLowerSearchAndRecutPriority(t *testing.T) {
	normalExposure, normalSharpness := 0.01, 500.0
	badExposure, badSharpness := 0.95, 0.0
	normal := rushestools.ShotCandidate{OverexposedRatio: &normalExposure, SharpnessScore: &normalSharpness}
	bad := rushestools.ShotCandidate{OverexposedRatio: &badExposure, SharpnessScore: &badSharpness}
	if shotQualityPenalty(normal) != 0 || shotQualityPenalty(bad) <= 0 || shotQualityPenalty(bad) > 0.22 {
		t.Fatalf("normal=%.4f bad=%.4f", shotQualityPenalty(normal), shotQualityPenalty(bad))
	}
	segments := []understanding.Segment{
		{SourceStartFrame: 0, SourceEndFrame: 90, OverexposedRatio: &badExposure, SharpnessScore: &badSharpness},
		{SourceStartFrame: 90, SourceEndFrame: 180, OverexposedRatio: &normalExposure, SharpnessScore: &normalSharpness},
	}
	ranges := beatMixRangesFromUnderstanding(segments, 180)
	if len(ranges) < 2 || ranges[0].StartFrame != 90 || ranges[0].QualityPenalty != 0 {
		t.Fatalf("ranges=%#v", ranges)
	}
	start, ok := chooseUnusedBeatMixSourceStart(180, 30, ranges, nil, 7, true)
	if !ok || start != 90 {
		t.Fatalf("start=%d ok=%v ranges=%#v", start, ok, ranges)
	}
}

func TestShotSearchRanksSegmentEvidenceAboveSharedFilename(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_segment_ranking")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	fixtures := []struct {
		assetID  string
		filename string
		segments []map[string]any
	}{
		{
			assetID: "video_year", filename: "Force Touch报道.mov",
			segments: []map[string]any{
				{"source_start_frame": 0, "source_end_frame": 101, "description": "2015年3月9日 Apple 发布全新 MacBook 新闻稿标题", "tags": []string{"MacBook", "新闻稿"}, "quality": "usable"},
				{"source_start_frame": 203, "source_end_frame": 304, "description": "高亮展示 Force Touch 触控板功能说明", "tags": []string{"Force Touch", "触控板"}, "quality": "usable"},
			},
		},
		{
			assetID: "video_backlight", filename: "键盘背光对比无动效.mov",
			segments: []map[string]any{
				{"source_start_frame": 0, "source_end_frame": 98, "description": "暖光明亮环境，清晰展示键盘布局", "lighting": []string{"均匀照明"}, "quality": "usable"},
				{"source_start_frame": 195, "source_end_frame": 293, "description": "环境骤暗，屏幕全黑，仅有键盘微弱反光", "lighting": []string{"极低照度", "暗光"}, "tags": []string{"全黑", "夜景"}, "quality": "dark"},
			},
		},
	}
	for _, fixture := range fixtures {
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO assets(
				asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
				probe_json,ingest_status,understanding_status,usable
			) VALUES(?, 'reference', ?, 'video', 'local_path', ?, ?, 1,
				'{"duration_sec":14}', 'ready', 'ready', 1);
			INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
			VALUES('draft_segment_ranking', ?, 'Broll', ?);`,
			fixture.assetID, "/tmp/"+fixture.filename, fixture.filename, fixture.assetID,
			fixture.assetID, now,
		); err != nil {
			t.Fatal(err)
		}
		summary, _ := json.Marshal(map[string]any{
			"asset_id": fixture.assetID, "analysis_depth": "deep", "semantic_role": "b_roll",
			"segments": fixture.segments,
		})
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO material_summaries(
				summary_id,asset_id,version,status,summary_json,fingerprint,prompt_version,created_at
			) VALUES(?, ?, 1, 'ready', ?, ?, 'v3', ?)`,
			"summary_"+fixture.assetID, fixture.assetID, string(summary), "fingerprint_"+fixture.assetID, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), "draft_segment_ranking")

	yearRaw, err := service.ExecuteTool(ctx, "media.search_shots", rushestools.ShotSearchInput{
		Query: "2015年 MacBook Force Touch 触控板 历史", AssetIDs: []string{"video_year"}, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	year := yearRaw.(rushestools.ShotSearchResult)
	if len(year.Shots) < 2 || year.Shots[0].SourceStartFrame != 0 || year.Shots[0].SegmentScore <= year.Shots[0].AssetScore {
		t.Fatalf("year ranking=%#v", year.Shots)
	}

	backlightRaw, err := service.ExecuteTool(ctx, "media.search_shots", rushestools.ShotSearchInput{
		Query: "键盘背光 无背光 晚上打字", AssetIDs: []string{"video_backlight"}, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	backlight := backlightRaw.(rushestools.ShotSearchResult)
	if len(backlight.Shots) < 2 || backlight.Shots[0].SourceStartFrame != 195 || backlight.Shots[0].SegmentScore <= backlight.Shots[1].SegmentScore {
		t.Fatalf("backlight ranking=%#v", backlight.Shots)
	}
}
