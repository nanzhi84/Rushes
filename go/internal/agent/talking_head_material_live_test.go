package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/providers"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

const defaultTalkingHeadMaterialRoot = "/Users/yoryon/影视飓风剪辑课/课程1-2 第二节课/第二节课-练习素材包"

type talkingHeadMaterialTrace struct {
	Tool  string `json:"tool"`
	Phase string `json:"phase"`
	Error string `json:"error,omitempty"`
}

type talkingHeadMaterialReport struct {
	GeneratedAt          string                     `json:"generated_at"`
	MaterialRoot         string                     `json:"material_root"`
	RoleCorrect          int                        `json:"role_correct"`
	RoleTotal            int                        `json:"role_total"`
	RoleAccuracy         float64                    `json:"role_accuracy"`
	UtteranceCount       int                        `json:"utterance_count"`
	PauseCount           int                        `json:"pause_count"`
	BrollCorrect         int                        `json:"broll_correct"`
	BrollTotal           int                        `json:"broll_total"`
	BrollAccuracy        float64                    `json:"broll_accuracy"`
	TimelineValid        bool                       `json:"timeline_valid"`
	ContextRunes         int                        `json:"context_runes"`
	FullTranscriptAbsent bool                       `json:"full_transcript_absent"`
	Trace                []talkingHeadMaterialTrace `json:"trace"`
}

func TestTalkingHeadRealMaterialAcceptance(t *testing.T) {
	if os.Getenv("RUSHES_TALKING_HEAD_EVAL") != "1" {
		t.Skip("设置 RUSHES_TALKING_HEAD_EVAL=1 才运行真实口播素材验收")
	}
	key := strings.TrimSpace(os.Getenv("RUSHES_DASHSCOPE_API_KEY"))
	if key == "" {
		t.Fatal("真实口播素材验收缺少 RUSHES_DASHSCOPE_API_KEY")
	}
	root := strings.TrimSpace(os.Getenv("RUSHES_TALKING_HEAD_MATERIAL_ROOT"))
	if root == "" {
		root = defaultTalkingHeadMaterialRoot
	}
	var err error
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatalf("解析真实口播素材根目录: %v", err)
	}
	videoPaths := realTalkingHeadVideos(t, root)
	report := talkingHeadMaterialReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), MaterialRoot: root,
		BrollTotal: 4, Trace: []talkingHeadMaterialTrace{},
	}
	for _, path := range videoPaths {
		relative, _ := filepath.Rel(root, path)
		relative = strings.ToLower(filepath.ToSlash(relative))
		expected := ""
		switch {
		case strings.Contains("/"+relative, "/aroll/"):
			expected = "a_roll"
		case strings.Contains("/"+relative, "/broll/"):
			expected = "b_roll"
		}
		if expected == "" {
			continue
		}
		report.RoleTotal++
		if understanding.SuggestVisualRole(filepath.Base(path), filepath.Dir(relative), "") == expected {
			report.RoleCorrect++
		}
	}
	report.RoleAccuracy = ratio(report.RoleCorrect, report.RoleTotal)
	if report.RoleAccuracy < liveToolStabilityTarget {
		t.Fatalf("真实素材 A/B-roll 角色识别率 %.2f%% 低于 95%%", report.RoleAccuracy*100)
	}

	selectedPaths := []string{filepath.Join(root, "视频/Aroll/Tim-Macbook Neo Talking节选.mp4")}
	for _, path := range videoPaths {
		relative, _ := filepath.Rel(root, path)
		if strings.Contains(strings.ToLower(filepath.ToSlash(relative)), "/broll/") {
			selectedPaths = append(selectedPaths, path)
		}
	}
	for _, path := range selectedPaths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("验收素材不可读 %s: %v", path, err)
		}
	}
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_talking_head_real")
	assetIDs := make([]string, 0, len(selectedPaths))
	for index, path := range selectedPaths {
		assetID := fmt.Sprintf("asset_real_%02d", index)
		assetIDs = append(assetIDs, assetID)
		probe, err := media.ProbeFile(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		relative, _ := filepath.Rel(root, path)
		probeJSON, _ := json.Marshal(probe)
		referencePath := path
		if index == 0 {
			// 避免同目录 SRT 让真实 ASR 验收走捷径。
			referencePath = filepath.Join(database.Paths.Temporary, "talking-head-live-no-sidecar.mp4")
			if err := os.Symlink(path, referencePath); err != nil {
				if linkErr := os.Link(path, referencePath); linkErr != nil {
					input, openErr := os.Open(path)
					if openErr != nil {
						t.Fatal(openErr)
					}
					output, createErr := os.Create(referencePath)
					if createErr != nil {
						_ = input.Close()
						t.Fatal(createErr)
					}
					_, copyErr := io.Copy(output, input)
					closeOutputErr := output.Close()
					closeInputErr := input.Close()
					if copyErr != nil || closeOutputErr != nil || closeInputErr != nil {
						_ = os.Remove(referencePath)
						t.Fatalf("复制无 sidecar 的 A-roll: copy=%v close_output=%v close_input=%v", copyErr, closeOutputErr, closeInputErr)
					}
				}
			}
		}
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO assets(
				asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
				probe_json,ingest_status,understanding_status,usable
			) VALUES(?, 'reference', ?, 'video', 'local_path', ?, ?, 1, ?, 'ready', 'none', 1);
			INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
			VALUES('draft_talking_head_real', ?, ?, ?);`,
			assetID, referencePath, filepath.Base(path), assetID, string(probeJSON),
			assetID, filepath.Dir(relative), time.Now().UTC().Add(time.Duration(index)*time.Millisecond).Format(time.RFC3339Nano),
		); err != nil {
			t.Fatal(err)
		}
	}
	tiers, err := providers.NewQwenTiers(t.Context(), providers.QwenTierConfig{
		APIKey: key, BaseURL: os.Getenv("RUSHES_DASHSCOPE_BASE_URL"),
		ChatModel: os.Getenv("RUSHES_QWEN_CHAT_MODEL"), VisionModel: os.Getenv("RUSHES_QWEN_VISION_MODEL"),
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewServiceWithModels(t.Context(), database, tiers.Chat, tiers.Vision)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	recognizer, err := providers.NewDashScopeASR(providers.DashScopeASRConfig{
		APIKey: key, BaseURL: os.Getenv("RUSHES_DASHSCOPE_ASR_BASE_URL"),
		Model: os.Getenv("RUSHES_DASHSCOPE_ASR_MODEL"),
	})
	if err != nil {
		t.Fatal(err)
	}
	service.SetSpeechRecognizer(recognizer)
	ctx := rushestools.WithDraftID(t.Context(), "draft_talking_head_real")
	ctx = rushestools.WithReporter(ctx, func(_ context.Context, name, phase string, _, _ any, toolErr error) {
		entry := talkingHeadMaterialTrace{Tool: name, Phase: phase}
		if toolErr != nil {
			entry.Error = toolErr.Error()
		}
		report.Trace = append(report.Trace, entry)
	})

	var understood rushestools.UnderstandResult
	// 该工具级质量验收没有启动 worker；显式 8 步的单素材 scan 与原 deep 使用同一分析步数，
	// 多素材/deep 的 queued→worker→bridge 链路由 internal/integration 独立验收。
	invokeRegisteredTool(t, service, ctx, "understand.materials", rushestools.UnderstandInput{
		AssetIDs: assetIDs[:1], Depth: "scan", Focus: "口播 A-roll；主体、表达主题和可剪辑语义",
		MaxStepsPerAsset: 8,
	}, &understood)
	if understood.Status != "completed" || len(understood.Summaries) != 1 {
		t.Fatalf("understanding=%#v", understood)
	}
	arollFrames := 0
	for _, summary := range understood.Summaries {
		if summary.AssetID != assetIDs[0] || summary.SemanticRole != "a_roll" {
			t.Fatalf("真实 A-roll summary=%#v", summary)
		}
		probe, _ := media.ProbeFile(t.Context(), selectedPaths[0])
		arollFrames = int(math.Round(probe.DurationSec * timeline.DefaultFPS))
	}
	var composed rushestools.ToolResult
	invokeRegisteredTool(t, service, ctx, "timeline.compose_initial", rushestools.ComposeInitialInput{
		Clips: []rushestools.ComposeClip{{
			AssetID: assetIDs[0], SourceStartFrame: 0, SourceEndFrame: arollFrames, Role: "a_roll",
		}},
	}, &composed)
	if composed.Status != "succeeded" {
		t.Fatalf("compose=%#v", composed)
	}
	var speech rushestools.SpeechInspectResult
	invokeRegisteredTool(t, service, ctx, "speech.inspect", rushestools.SpeechInspectInput{
		TimelineClipID: "clip_v1_001", MaxUtterances: 120,
		IncludeWords: true, MaxWords: 2000,
	}, &speech)
	report.UtteranceCount, report.PauseCount = speech.UtteranceTotal, len(speech.Pauses)
	if !strings.HasPrefix(speech.ProviderID, providers.DefaultASRModel+"+") ||
		speech.UtteranceTotal < 10 || speech.WordTotal < 100 || len(speech.Pauses) == 0 {
		t.Fatalf("speech=%#v", speech)
	}

	queries := []struct {
		query string
		want  string
	}{
		{query: "柑橘色 外观 颜色展示", want: "柑橘色"},
		{query: "键盘 没有背光 对比", want: "背光"},
		{query: "指纹识别 解锁 键盘", want: "指纹"},
		{query: "触控板 物理按压 特写", want: "触控板"},
	}
	var fingerprintShot rushestools.ShotCandidate
	for _, query := range queries {
		var discovery rushestools.ShotSearchResult
		invokeRegisteredTool(t, service, ctx, "media.search_shots", rushestools.ShotSearchInput{
			Query: query.query, SemanticRoles: []string{"b_roll"}, MinDurationFrames: 45, Limit: 5,
		}, &discovery)
		if len(discovery.UnderstandingCandidates) == 0 ||
			!strings.Contains(discovery.UnderstandingCandidates[0].Filename, query.want) {
			t.Fatalf("query=%q discovery=%#v", query.query, discovery)
		}
		var onDemand rushestools.UnderstandResult
		invokeRegisteredTool(t, service, ctx, "understand.materials", rushestools.UnderstandInput{
			AssetIDs: []string{discovery.UnderstandingCandidates[0].AssetID},
			Depth:    "scan", Focus: query.query, MaxStepsPerAsset: 8,
		}, &onDemand)
		if onDemand.Status != "completed" || len(onDemand.Summaries) != 1 {
			t.Fatalf("query=%q on-demand understanding=%#v", query.query, onDemand)
		}
		var search rushestools.ShotSearchResult
		invokeRegisteredTool(t, service, ctx, "media.search_shots", rushestools.ShotSearchInput{
			Query: query.query, SemanticRoles: []string{"b_roll"}, MinDurationFrames: 45, Limit: 5,
		}, &search)
		if len(search.Shots) > 0 {
			t.Logf("BROLL_QUERY query=%q want=%q top=%q terms=%v", query.query, query.want, search.Shots[0].Filename, search.Shots[0].MatchedQueryTerms)
		} else {
			t.Logf("BROLL_QUERY query=%q want=%q top=<none>", query.query, query.want)
		}
		if len(search.Shots) > 0 && strings.Contains(search.Shots[0].Filename, query.want) {
			report.BrollCorrect++
		}
		if query.want == "指纹" && len(search.Shots) > 0 {
			fingerprintShot = search.Shots[0]
		}
	}
	report.BrollAccuracy = ratio(report.BrollCorrect, report.BrollTotal)
	if report.BrollAccuracy < liveToolStabilityTarget {
		t.Fatalf("真实 B-roll 语义检索率 %.2f%% 低于 95%%", report.BrollAccuracy*100)
	}
	fingerprintUtterance := ""
	fingerprintStartWord := ""
	fingerprintEndWord := ""
	for _, utterance := range speech.Utterances {
		if strings.Contains(utterance.Text, "指纹") {
			fingerprintUtterance = utterance.UtteranceID
			fingerprintStartWord, fingerprintEndWord, _ = semanticWordRange(
				utterance.Words, "指纹", fingerprintShot.DurationFrames,
			)
			break
		}
	}
	if fingerprintUtterance == "" || fingerprintStartWord == "" || fingerprintShot.ShotID == "" {
		t.Fatal("没有取得指纹台词、词级语义锚点或对应 B-roll 镜头")
	}
	var edited rushestools.ToolResult
	invokeRegisteredTool(t, service, ctx, "timeline.edit_talking_head", rushestools.TalkingHeadEditInput{
		ARollTimelineClipID: "clip_v1_001", RemovePauseIDs: []string{speech.Pauses[0].PauseID},
		BrollAssignments: []rushestools.TalkingHeadBrollAssignment{{
			ShotID: fingerprintShot.ShotID, StartWordID: fingerprintStartWord, EndWordID: fingerprintEndWord,
		}},
	}, &edited)
	if edited.Status != "succeeded" {
		t.Fatalf("edit=%#v", edited)
	}
	latest, err := timeline.Latest(t.Context(), database, "draft_talking_head_real")
	if err != nil {
		t.Fatal(err)
	}
	report.TimelineValid = timeline.Validate(latest).Valid
	contextText, err := NewContextBuilder(database).Build(t.Context(), "draft_talking_head_real")
	if err != nil {
		t.Fatal(err)
	}
	report.ContextRunes = len([]rune(contextText))
	report.FullTranscriptAbsent = !strings.Contains(contextText, "这个全新的柑橘色") &&
		strings.Contains(contextText, `"speech_searchable":true`)
	if !report.TimelineValid || !report.FullTranscriptAbsent {
		t.Fatalf("timeline_valid=%v full_transcript_absent=%v", report.TimelineValid, report.FullTranscriptAbsent)
	}
	writeTalkingHeadMaterialReport(t, report)
	t.Logf(
		"TALKING_HEAD_ACCEPTANCE role=%d/%d broll=%d/%d utterances=%d pauses=%d timeline_valid=%v context_runes=%d trace_events=%d",
		report.RoleCorrect, report.RoleTotal, report.BrollCorrect, report.BrollTotal,
		report.UtteranceCount, report.PauseCount, report.TimelineValid, report.ContextRunes, len(report.Trace),
	)
}

func semanticWordRange(
	words []rushestools.SpeechWordEvidence,
	target string,
	maxDurationFrames int,
) (string, string, bool) {
	target = agentexec.NormalizeSpeechText(target)
	for start := range words {
		text := ""
		for end := start; end < len(words); end++ {
			if maxDurationFrames > 0 && words[end].SourceEndFrame-words[start].SourceStartFrame > maxDurationFrames {
				break
			}
			text += words[end].Text + words[end].Punctuation
			if strings.Contains(agentexec.NormalizeSpeechText(text), target) {
				return words[start].WordID, words[end].WordID, true
			}
		}
	}
	return "", "", false
}

func realTalkingHeadVideos(t *testing.T, root string) []string {
	t.Helper()
	result := []string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		extension := strings.ToLower(filepath.Ext(path))
		if extension == ".mp4" || extension == ".mov" {
			result = append(result, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(result)
	if len(result) == 0 {
		t.Fatal("真实素材目录没有视频")
	}
	return result
}

func invokeRegisteredTool[I, O any](
	t *testing.T, service *Service, ctx context.Context, name string, input I, output *O,
) {
	t.Helper()
	var invokable einotool.InvokableTool
	for _, spec := range service.tools.Specs(true) {
		if spec.Name == name {
			invokable, _ = spec.Implementation.(einotool.InvokableTool)
			break
		}
	}
	if invokable == nil {
		t.Fatalf("工具未注册: %s", name)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := invokable.InvokableRun(ctx, string(encoded))
	if err != nil {
		t.Fatalf("tool=%s err=%v raw=%s", name, err, raw)
	}
	if err := json.Unmarshal([]byte(raw), output); err != nil {
		t.Fatalf("tool=%s decode=%v raw=%s", name, err, raw)
	}
}

func writeTalkingHeadMaterialReport(t *testing.T, report talkingHeadMaterialReport) {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("RUSHES_TALKING_HEAD_EVAL_REPORT"))
	if path == "" {
		return
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
