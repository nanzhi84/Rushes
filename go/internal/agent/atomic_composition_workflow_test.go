package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestInitialCompositionFixtureUsesSearchAndAtomicInserts(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_initial_atomic_fixture"
	agenttest.CreateAgentDraft(t, database, draftID)
	seedAtomicWorkflowVideo(t, database, draftID, "initial_city", "城市街景", "城市 夜景", 60)
	seedAtomicWorkflowVideo(t, database, draftID, "initial_mountain", "山间航拍", "山峰 云海", 60)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	trace := []string{}

	listedRaw, err := service.ExecuteTool(ctx, "asset.list_assets", rushestools.AssetListInput{})
	if err != nil {
		t.Fatal(err)
	}
	trace = append(trace, "asset.list_assets")
	if len(listedRaw.(rushestools.AssetListResult).Assets) != 2 {
		t.Fatalf("assets=%#v", listedRaw)
	}
	searchRaw, err := service.ExecuteTool(ctx, "shot.search", rushestools.ShotSearchInput{
		Query: "城市 山峰", SemanticRoles: []string{"b_roll"}, MinDurationFrames: 45, Limit: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	trace = append(trace, "shot.search")
	search := searchRaw.(rushestools.ShotSearchResult)
	if len(search.Shots) < 2 {
		t.Fatalf("shot.search=%#v", search)
	}
	for _, shot := range search.Shots[:2] {
		raw, executeErr := service.ExecuteTool(ctx, "timeline.insert", rushestools.TimelineInsertInput{
			"kind": "insert_clip", "asset_id": shot.AssetID, "role": "b_roll",
			"source_start_frame": shot.SourceStartFrame,
			"source_end_frame":   shot.SourceEndFrame,
			"metadata":           map[string]any{"shot_id": shot.ShotID},
		})
		if executeErr != nil || raw.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
			t.Fatalf("timeline.insert=%#v err=%v", raw, executeErr)
		}
		trace = append(trace, "timeline.insert")
	}
	checkRaw, err := service.ExecuteTool(ctx, "timeline.check", rushestools.TimelineCheckInput{})
	if err != nil || checkRaw.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("timeline.check=%#v err=%v", checkRaw, err)
	}
	trace = append(trace, "timeline.check")

	document, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || document.Version != 2 ||
		len(timelineTrackClips(document, "visual_base")) != 2 ||
		!timeline.Validate(document).Valid {
		t.Fatalf("document=%#v err=%v", document, err)
	}
	if slices.Contains(trace, "timeline.compose_initial") {
		t.Fatalf("initial trace used composite tool: %v", trace)
	}
	for _, want := range []string{"asset.list_assets", "shot.search", "timeline.insert", "timeline.check"} {
		if !slices.Contains(trace, want) {
			t.Fatalf("initial trace=%v missing %s", trace, want)
		}
	}
}

func TestBeatCompositionFixtureUsesDetectorSearchAndAtomicEdits(t *testing.T) {
	fakeBin := t.TempDir()
	for name, body := range map[string]string{
		"aubiotrack": "#!/bin/sh\nprintf '0.000000\\n1.000000\\n2.000000\\n3.000000\\n4.000000\\n'\n",
		"aubioonset": "#!/bin/sh\nprintf '0.000000\\n2.000000\\n4.000000\\n'\n",
	} {
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_beat_atomic_fixture"
	agenttest.CreateAgentDraft(t, database, draftID)
	seedAtomicWorkflowVideo(t, database, draftID, "beat_city", "城市推进镜头", "城市 推进", 60)
	seedAtomicWorkflowVideo(t, database, draftID, "beat_mountain", "山峰揭示镜头", "山峰 揭示", 60)
	audioPath := filepath.Join(database.Paths.Temporary, "beat-fixture.wav")
	if _, err := media.RunCommand(
		t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i",
		"sine=frequency=440:sample_rate=44100:duration=4", "-c:a", "pcm_s16le", audioPath,
	); err != nil {
		t.Fatal(err)
	}
	seedAtomicWorkflowAudio(t, database, draftID, "beat_bgm", audioPath, 4, "BGM")
	seedAtomicWorkflowAudio(t, database, draftID, "beat_sfx", audioPath, 1, "SFX")

	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	trace := []string{}

	beatsRaw, err := service.ExecuteTool(ctx, "audio.analyze_beats", rushestools.AudioBeatAnalysisInput{
		AssetID: "beat_bgm", MaxBeats: 64, WaveformPoints: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	trace = append(trace, "audio.analyze_beats")
	beats := beatsRaw.(rushestools.AudioBeatAnalysisResult)
	if beats.DurationFrames != 120 || !slices.Contains(beats.BeatFrames, 60) ||
		len(beats.Waveform.Samples) == 0 {
		t.Fatalf("beats=%#v", beats)
	}
	searchRaw, err := service.ExecuteTool(ctx, "shot.search", rushestools.ShotSearchInput{
		Query: "城市 山峰", SemanticRoles: []string{"b_roll"}, MinDurationFrames: 60, Limit: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	trace = append(trace, "shot.search")
	search := searchRaw.(rushestools.ShotSearchResult)
	if len(search.Shots) < 2 {
		t.Fatalf("shot.search=%#v", search)
	}
	for _, shot := range search.Shots[:2] {
		raw, executeErr := service.ExecuteTool(ctx, "timeline.insert", rushestools.TimelineInsertInput{
			"kind": "insert_clip", "asset_id": shot.AssetID, "role": "b_roll",
			"source_start_frame": shot.SourceStartFrame,
			"source_end_frame":   shot.SourceEndFrame,
			"metadata":           map[string]any{"shot_id": shot.ShotID},
		})
		if executeErr != nil || raw.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
			t.Fatalf("visual insert=%#v err=%v", raw, executeErr)
		}
		trace = append(trace, "timeline.insert")
	}
	beatGrid := map[string]any{
		"bpm": beats.BPM, "beat_frames": beats.BeatFrames,
		"strong_beat_frames": beats.StrongBeatFrames,
		"downbeat_frames":    beats.DownbeatFrames,
		"bar_phase":          beats.BarPhase,
		"analysis_method":    beats.AnalysisMethod,
	}
	bgmRaw, err := service.ExecuteTool(ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "track_id": "bgm", "asset_id": "beat_bgm", "role": "bgm",
		"timeline_start_frame": 0, "source_start_frame": 0, "source_end_frame": 120,
		"metadata": map[string]any{"beat_grid": beatGrid},
	})
	if err != nil || bgmRaw.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("bgm insert=%#v err=%v", bgmRaw, err)
	}
	trace = append(trace, "timeline.insert")
	sfxRaw, err := service.ExecuteTool(ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "track_id": "sfx", "asset_id": "beat_sfx", "role": "sfx",
		"timeline_start_frame": 60, "source_start_frame": 0, "source_end_frame": 30,
	})
	if err != nil || sfxRaw.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("sfx insert=%#v err=%v", sfxRaw, err)
	}
	trace = append(trace, "timeline.insert")
	withSFX, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || len(timelineTrackClips(withSFX, "sfx")) != 1 {
		t.Fatalf("with sfx=%#v err=%v", withSFX, err)
	}
	sfxID := timelineTrackClips(withSFX, "sfx")[0].TimelineClipID
	gainRaw, err := service.ExecuteTool(ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "adjust_gain", "timeline_clip_id": sfxID, "gain_db": -12,
	})
	if err != nil || gainRaw.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("sfx gain=%#v err=%v", gainRaw, err)
	}
	trace = append(trace, "timeline.update")
	checkRaw, err := service.ExecuteTool(ctx, "timeline.check", rushestools.TimelineCheckInput{})
	if err != nil || checkRaw.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("timeline.check=%#v err=%v", checkRaw, err)
	}
	trace = append(trace, "timeline.check")
	alignment, _ := checkRaw.(rushestools.ToolResult).Data["beat_alignment"].(map[string]any)
	if alignment["beat_grid_present"] != true ||
		alignment["all_cuts_on_beat_grid"] != true ||
		alignment["on_beat_cut_count"] != 1 {
		t.Fatalf("alignment=%#v", alignment)
	}

	final, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || final.Version != 5 || !timeline.Validate(final).Valid ||
		len(timelineTrackClips(final, "bgm")) != 1 ||
		len(timelineTrackClips(final, "sfx")) != 1 ||
		timelineTrackClips(final, "sfx")[0].GainDB != -12 {
		t.Fatalf("final=%#v err=%v", final, err)
	}
	if slices.Contains(trace, "timeline.recut_to_beats") {
		t.Fatalf("beat trace used composite tool: %v", trace)
	}
	var singleOperationBatches int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM timeline_edit_batches
		WHERE draft_id=? AND json_array_length(operations_json)=1`, draftID,
	).Scan(&singleOperationBatches); err != nil || singleOperationBatches != 5 {
		t.Fatalf("single-op batches=%d err=%v trace=%v", singleOperationBatches, err, trace)
	}
}

func seedAtomicWorkflowVideo(
	t *testing.T,
	database *storage.DB,
	draftID string,
	assetID string,
	description string,
	tags string,
	durationFrames int,
) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	probe, err := json.Marshal(map[string]any{
		"duration_sec": float64(durationFrames) / 30, "has_audio": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES(?, 'reference', ?, 'video', 'local_path', ?, ?, 1, ?, 'ready', 'ready', 1)`,
		assetID, "/tmp/"+assetID+".mp4", assetID+".mp4", assetID, string(probe),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
		VALUES(?, ?, 'Broll', ?)`, draftID, assetID, now,
	); err != nil {
		t.Fatal(err)
	}
	summary, err := json.Marshal(map[string]any{
		"asset_id": assetID, "semantic_role": "b_roll", "overall": description,
		"segments": []map[string]any{{
			"source_start_frame": 0, "source_end_frame": durationFrames,
			"description": description, "tags": []string{tags}, "quality": "usable",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO material_summaries(
			summary_id,asset_id,version,status,summary_json,fingerprint,prompt_version,created_at
		) VALUES(?, ?, 1, 'ready', ?, ?, 'atomic-fixture-v1', ?)`,
		"summary_"+assetID, assetID, string(summary), "fingerprint_"+assetID, now,
	); err != nil {
		t.Fatal(err)
	}
}

func seedAtomicWorkflowAudio(
	t *testing.T,
	database *storage.DB,
	draftID string,
	assetID string,
	path string,
	durationSeconds int,
	relDir string,
) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := json.Marshal(map[string]any{
		"duration_sec": float64(durationSeconds), "has_audio": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES(?, 'reference', ?, 'audio', 'local_path', ?, ?, ?, ?, 'ready', 'none', 1)`,
		assetID, path, filepath.Base(path), assetID, info.Size(), string(probe),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
		VALUES(?, ?, ?, ?)`, draftID, assetID, relDir, now,
	); err != nil {
		t.Fatal(err)
	}
}
