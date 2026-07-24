package agentexec

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// TestToolEffectMatchesExecutorWriteFootprint 对拍工具的 Effect 分级与执行器真实写行为
// （#103 G1 验收）：全部只读工具执行后事件日志零增长且业务表快照不变；可逆工具经 reducer 落盘（事件或 ResultRows）；
// 破坏性 memory.update 的 remove_keys 真正删记忆。Effect 事实源取自由同一执行器构造的注册表，
// 与被执行的方法闭环对拍。
func TestToolEffectMatchesExecutorWriteFootprint(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := rushestools.NewRegistry(database, exec)
	if err != nil {
		t.Fatal(err)
	}
	effectOf := func(name string) rushestools.Effect {
		effect, ok := registry.Effect(name)
		if !ok {
			t.Fatalf("%s 未注册 Effect", name)
		}
		return effect
	}
	countEvents := func(draftID string) int {
		events, err := storage.ListEventsAfter(t.Context(), database.Read(), 0, &draftID, 100000)
		if err != nil {
			t.Fatal(err)
		}
		return len(events)
	}

	t.Run("all read-only tools leave every business table untouched", func(t *testing.T) {
		const draftID = "draft_effect_readonly"
		agenttest.CreateAgentDraft(t, database, draftID)
		ctx := rushestools.WithDraftID(t.Context(), draftID)
		_, ffmpegErr := osexec.LookPath("ffmpeg")
		_, aubioErr := osexec.LookPath("aubiotrack")

		audioAssetID := ""
		if ffmpegErr == nil {
			audioAssetID = setupReadOnlyAudioFixture(t, database, draftID)
		}
		previewID := ""
		if ffmpegErr == nil {
			previewID = setupReadOnlyPreviewFixture(t, database, exec, draftID)
		}

		type readOnlyCase struct {
			input      any
			skipReason string
		}
		cases := map[string]readOnlyCase{
			"asset.list_assets": {input: rushestools.AssetListInput{}},
			"timeline.inspect":  {input: rushestools.TimelineInspectInput{}},
			"shot.search":       {input: rushestools.ShotSearchInput{Query: "人物"}},
			"render.status":     {input: rushestools.RenderStatusInput{}},
			"preview.check": {
				input:      rushestools.PreviewCheckInput{PreviewID: previewID, Check: "decode"},
				skipReason: dependencySkipReason(ffmpegErr, "ffmpeg"),
			},
			"speech.search": {
				input:      rushestools.SpeechSearchInput{AssetID: audioAssetID},
				skipReason: dependencySkipReason(ffmpegErr, "ffmpeg"),
			},
			"timeline.check": {input: rushestools.TimelineCheckInput{}},
			"audio.analyze_beats": {
				input: rushestools.AudioBeatAnalysisInput{AssetID: audioAssetID, MaxBeats: 32, WaveformPoints: 32},
				skipReason: firstNonEmptySkip(
					dependencySkipReason(ffmpegErr, "ffmpeg"), dependencySkipReason(aubioErr, "aubio"),
				),
			},
			"audio.analyze_speech_pauses": {
				input:      rushestools.SpeechPauseAnalysisInput{AssetID: audioAssetID, IncludeBoundaries: true},
				skipReason: dependencySkipReason(ffmpegErr, "ffmpeg"),
			},
		}

		registered := make([]string, 0, len(cases))
		for _, spec := range registry.Specs(true) {
			if spec.Effect == rushestools.EffectReadOnly {
				registered = append(registered, spec.Name)
			}
		}
		sort.Strings(registered)
		caseNames := make([]string, 0, len(cases))
		for name := range cases {
			caseNames = append(caseNames, name)
		}
		sort.Strings(caseNames)
		if strings.Join(registered, "|") != strings.Join(caseNames, "|") {
			t.Fatalf("read_only registry=%v parity cases=%v；新增只读工具必须登记写足迹用例", registered, caseNames)
		}

		for _, name := range caseNames {
			test := cases[name]
			t.Run(name, func(t *testing.T) {
				if test.skipReason != "" {
					t.Skip(test.skipReason)
				}
				beforeEvents := countEvents(draftID)
				before := databaseBusinessSnapshot(t, database)
				if _, err := exec.ExecuteTool(ctx, name, test.input); err != nil {
					t.Fatal(err)
				}
				if afterEvents := countEvents(draftID); afterEvents != beforeEvents {
					t.Fatalf("%s 写入 event_log: before=%d after=%d", name, beforeEvents, afterEvents)
				}
				if after := databaseBusinessSnapshot(t, database); after != before {
					t.Fatalf("%s 改变了业务表快照", name)
				}
			})
		}
	})

	t.Run("reversible plan.update persists through the reducer result rows", func(t *testing.T) {
		const draftID = "draft_effect_plan"
		agenttest.CreateAgentDraft(t, database, draftID)
		if effect := effectOf("plan.update"); effect != rushestools.EffectReversible {
			t.Fatalf("plan.update Effect=%q，期望 reversible", effect)
		}
		ctx := rushestools.WithDraftID(t.Context(), draftID)
		if _, err := exec.toolPlanUpdate(ctx, draftID, rushestools.PlanUpdateInput{
			Plan: map[string]any{"pacing": "fast"},
		}); err != nil {
			t.Fatal(err)
		}
		draft, err := storage.GetDraft(t.Context(), database.Read(), draftID)
		if err != nil {
			t.Fatal(err)
		}
		if draft.ContentPlan["pacing"] != "fast" {
			t.Fatalf("plan.update 未落盘创作计划: %#v", draft.ContentPlan)
		}
	})

	t.Run("destructive memory.update remove_keys deletes the stored memory", func(t *testing.T) {
		const draftID = "draft_effect_memory"
		agenttest.CreateAgentDraft(t, database, draftID)
		agenttest.InsertAgentMessage(t, database, draftID, "message_effect_memory", "以后都快一点")
		if effect := effectOf("memory.update"); effect != rushestools.EffectDestructive {
			t.Fatalf("memory.update Effect=%q，期望 destructive", effect)
		}
		ctx := rushestools.WithDraftID(
			WithMemoryEvidence(t.Context(), storage.UserMemoryEvidenceMessage, "message_effect_memory"),
			draftID,
		)
		if _, err := exec.toolMemoryUpdate(ctx, draftID, rushestools.MemoryUpdateInput{
			Entries: []rushestools.MemoryEntryInput{{
				Key: "pacing", Kind: "preference", Statement: "成片节奏偏快", EvidenceQuote: "都快一点",
			}},
		}); err != nil {
			t.Fatal(err)
		}
		if memories, err := storage.ListUserMemories(t.Context(), database.Read()); err != nil || len(memories) != 1 {
			t.Fatalf("新增记忆未落盘: memories=%#v err=%v", memories, err)
		}
		if _, err := exec.toolMemoryUpdate(ctx, draftID, rushestools.MemoryUpdateInput{
			RemoveKeys: []string{"pacing"},
		}); err != nil {
			t.Fatal(err)
		}
		if memories, err := storage.ListUserMemories(t.Context(), database.Read()); err != nil || len(memories) != 0 {
			t.Fatalf("remove_keys 未删除记忆: memories=%#v err=%v", memories, err)
		}
	})
}

func dependencySkipReason(err error, dependency string) string {
	if err == nil {
		return ""
	}
	return dependency + " 未安装；CI 默认镜像会安装并执行本用例"
}

func firstNonEmptySkip(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func setupReadOnlyAudioFixture(t *testing.T, database *storage.DB, draftID string) string {
	t.Helper()
	const assetID = "asset_effect_audio"
	path := filepath.Join(database.Paths.Temporary, "effect-metronome.wav")
	if _, err := media.RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i",
		`aevalsrc=if(lt(mod(t\,0.5)\,0.03)\,0.9*sin(2*PI*1000*t)\,0):s=44100:d=5`,
		"-c:a", "pcm_s16le", path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": assetID, "job_id": "job_effect_audio", "storage_mode": "reference",
			"reference_path": path, "kind": "audio", "source": "local_path",
			"filename": filepath.Base(path), "hash": "effect_audio_hash", "size": info.Size(),
			"ingest_status": "ready", "usable": true,
			"probe": map[string]any{"duration_sec": 5, "has_audio": true},
		}},
		{Type: "AssetLinked", DraftID: draftID, Payload: map[string]any{"asset_id": assetID}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("audio fixture status=%s err=%v", result.Status, err)
	}
	return assetID
}

func setupReadOnlyPreviewFixture(
	t *testing.T,
	database *storage.DB,
	executor *Executor,
	draftID string,
) string {
	t.Helper()
	const assetID = "asset_effect_video"
	const previewID = "preview_effect_readonly"
	path := filepath.Join(database.Paths.Temporary, "effect-preview.mp4")
	if _, err := media.RunCommand(
		t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i",
		"testsrc2=size=64x64:rate=30:duration=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", path,
	); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": assetID, "job_id": "job_effect_video", "storage_mode": "reference",
			"reference_path": path, "kind": "video", "source": "local_path",
			"filename": filepath.Base(path), "hash": "effect_video_hash", "size": info.Size(),
			"ingest_status": "ready", "usable": true,
			"probe": map[string]any{"duration_sec": 1, "has_audio": false, "fps": 30},
		}},
		{Type: "AssetLinked", DraftID: draftID, Payload: map[string]any{"asset_id": assetID}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("video fixture status=%s err=%v", result.Status, err)
	}
	document, err := timeline.ComposeInitial(draftID, 1, []timeline.Selection{{
		AssetID: assetID, AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 30, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.PersistTimeline(t.Context(), draftID, document, "effect_readonly_fixture"); err != nil {
		t.Fatal(err)
	}
	object, err := media.NewObjectStore(database.Paths).PutFile(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	result, err = reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "PreviewRendered", DraftID: draftID, Payload: map[string]any{
			"artifact_id": previewID, "timeline_version": 1,
			"object_hash": object.Hash, "object_size": object.Size,
			"render_width": 64, "render_height": 64, "render_fps": 30,
			"expected_duration_sec": 1,
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("preview fixture status=%s err=%v", result.Status, err)
	}
	return previewID
}

func databaseBusinessSnapshot(t *testing.T, database *storage.DB) [sha256.Size]byte {
	t.Helper()
	tablesRows, err := database.Read().QueryContext(t.Context(), `
		SELECT name FROM sqlite_schema
		WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for tablesRows.Next() {
		var table string
		if err := tablesRows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := tablesRows.Close(); err != nil {
		t.Fatal(err)
	}
	result := sha256.New()
	for _, table := range tables {
		writeTableSnapshot(t, database, result, table)
	}
	var snapshot [sha256.Size]byte
	copy(snapshot[:], result.Sum(nil))
	return snapshot
}

func writeTableSnapshot(t *testing.T, database *storage.DB, output hash.Hash, table string) {
	t.Helper()
	quoted := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
	rows, err := database.Read().QueryContext(t.Context(), "SELECT * FROM "+quoted+" ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(output, "table:%s|columns:%s\n", table, strings.Join(columns, ","))
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for index := range values {
			targets[index] = &values[index]
		}
		if err := rows.Scan(targets...); err != nil {
			t.Fatal(err)
		}
		for _, value := range values {
			switch typed := value.(type) {
			case []byte:
				_, _ = fmt.Fprintf(output, "bytes:%x|", typed)
			default:
				_, _ = fmt.Fprintf(output, "%T:%v|", value, value)
			}
		}
		_, _ = output.Write([]byte{'\n'})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
