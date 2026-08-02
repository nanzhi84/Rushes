package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestHarnessPersistsOnDemandAudioStepsAndOrdinaryImportStaysLazy(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const (
		draftID = "draft_harness_audio_analysis"
		assetID = "asset_harness_audio_analysis"
	)
	agenttest.CreateAgentDraft(t, database, draftID)
	audioPath := filepath.Join(database.Paths.Temporary, "harness-audio.wav")
	if _, err := media.RunCommand(
		t.Context(), "ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=16000:duration=2",
		"-c:a", "pcm_s16le", audioPath,
	); err != nil {
		t.Fatal(err)
	}
	srtPath := strings.TrimSuffix(audioPath, filepath.Ext(audioPath)) + ".srt"
	if err := os.WriteFile(srtPath, []byte(
		"1\n00:00:00,100 --> 00:00:00,800\n第一句口播\n\n"+
			"2\n00:00:01,000 --> 00:00:01,800\n第二句口播\n\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	agenttest.InsertSpeechFixtureAsset(t, database, draftID, assetID, audioPath)
	var lazyCount int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM asset_analyses",
	).Scan(&lazyCount); err != nil || lazyCount != 0 {
		t.Fatalf("ordinary import analyses=%d err=%v", lazyCount, err)
	}

	fakeBin := t.TempDir()
	for name, body := range map[string]string{
		"aubiotrack": "#!/bin/sh\nprintf '0.25\\n0.50\\n0.75\\n1.00\\n1.25\\n1.50\\n'\n",
		"aubioonset": "#!/bin/sh\nprintf '0.25\\n0.75\\n1.25\\n'\n",
	} {
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	if err := service.prepareOnDemandAudioAnalysis(
		ctx, draftID, "timeline.insert", rushestools.TimelineInsertInput{
			"kind": "insert_clip", "track_id": "bgm", "asset_id": assetID,
			"source_start_frame": 0, "source_end_frame": 60,
		},
	); err != nil {
		t.Fatal(err)
	}
	if err := service.ensureExplicitBeatTaskAnalysis(
		ctx, draftID, "请用这首音乐完成卡点剪辑",
	); err != nil {
		t.Fatal(err)
	}
	document := timeline.Empty(draftID, 1)
	document.DurationFrames = 60
	document.Tracks[0].Clips = []timeline.Clip{{
		TimelineClipID: "clip_visual_existing", TrackID: "visual_base",
		AssetID: "visual_existing", AssetKind: "video", Role: "b_roll",
		TimelineStartFrame: 0, TimelineEndFrame: 60,
		SourceStartFrame: 0, SourceEndFrame: 60, PlaybackRate: 1,
	}}
	document.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "clip_bgm_existing", TrackID: "bgm",
		AssetID: assetID, AssetKind: "audio", Role: "bgm",
		TimelineStartFrame: 0, TimelineEndFrame: 60,
		SourceStartFrame: 0, SourceEndFrame: 60, PlaybackRate: 1,
	}}
	manualContext := rushestools.WithTimelineMutationOrigin(t.Context(), "manual")
	if seeded, seedErr := seedTimelineVersion(
		service, manualContext, draftID, document, "harness_audio_fixture", nil,
	); seedErr != nil || seeded.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("seeded=%#v err=%v", seeded, seedErr)
	}
	if err := service.prepareOnDemandAudioAnalysis(
		ctx, draftID, "timeline.update", rushestools.TimelineUpdateInput{
			"kind": "replace_clip", "timeline_clip_id": "clip_bgm_existing",
			"asset_id": assetID,
		},
	); err != nil {
		t.Fatal(err)
	}
	rawSearch, err := service.ExecuteTool(ctx, "speech.search", rushestools.SpeechSearchInput{
		AssetID: assetID, Query: "第二句", IncludeWords: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	search := rawSearch.(rushestools.SpeechSearchResult)
	foundSecond := false
	for _, utterance := range search.Utterances {
		foundSecond = foundSecond || strings.Contains(utterance.Text, "第二句")
	}
	if !foundSecond {
		t.Fatalf("search=%#v", search)
	}
	rawPause, err := service.ExecuteTool(
		ctx, "audio.analyze_speech_pauses", rushestools.SpeechPauseAnalysisInput{AssetID: assetID},
	)
	if err != nil {
		t.Fatal(err)
	}
	pause := rawPause.(rushestools.SpeechPauseAnalysisResult)
	if !strings.HasPrefix(pause.AnalysisMethod, "transcript-vad-v1/sidecar-srt") {
		t.Fatalf("pause did not reuse transcript VAD: %#v", pause)
	}

	events, _, unsubscribe := service.Hub().Subscribe(draftID)
	unsubscribe()
	for _, toolName := range []string{"audio.analyze_beats", "speech.transcribe"} {
		started, progressed, finished := false, false, false
		finishedCount := 0
		for _, event := range events {
			if event["tool"] != toolName || event["harness_owned"] != true {
				continue
			}
			switch event["type"] {
			case TurnStreamToolStepStarted:
				started = event["progress"] == 0
			case TurnStreamToolStepProgress:
				progressed = event["progress"] == 0.5
			case TurnStreamToolStepFinished:
				finished = event["status"] == "succeeded" &&
					event["progress"] == 1 && event["duration_ms"] != nil
				if finished {
					finishedCount++
				}
			}
		}
		if !started || !progressed || !finished {
			t.Fatalf(
				"tool=%s started=%v progressed=%v finished=%v events=%#v",
				toolName, started, progressed, finished, events,
			)
		}
		if toolName == "audio.analyze_beats" && finishedCount != 3 {
			t.Fatalf("BGM insert, explicit task, and BGM replace should expose Harness steps: %d", finishedCount)
		}
	}

	messages, err := storage.ListMessages(t.Context(), database.Read(), draftID, 20)
	if err != nil {
		t.Fatal(err)
	}
	traces := map[string]bool{}
	for _, message := range messages {
		if message.Kind != "tool" {
			continue
		}
		var trace map[string]any
		if json.Unmarshal([]byte(message.Content), &trace) == nil &&
			trace["harness_owned"] == true && trace["status"] == "succeeded" &&
			trace["progress"] == float64(1) && trace["duration_ms"] != nil {
			traces[trace["tool"].(string)] = true
		}
	}
	if !traces["audio.analyze_beats"] || !traces["speech.transcribe"] {
		t.Fatalf("persisted harness traces=%#v messages=%#v", traces, messages)
	}
	var beatCount, transcriptCount, pauseCount int
	for analysisType, target := range map[string]*int{
		"beat_grid": &beatCount, "transcript": &transcriptCount, "speech_pause": &pauseCount,
	} {
		if err := database.Read().QueryRowContext(t.Context(),
			"SELECT COUNT(*) FROM asset_analyses WHERE analysis_type=?", analysisType,
		).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if beatCount != 1 || transcriptCount != 1 || pauseCount != 2 {
		t.Fatalf("beat=%d transcript=%d pause=%d", beatCount, transcriptCount, pauseCount)
	}
}
