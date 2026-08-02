package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestAutomaticPreviewQARunsFiveCoreChecksInParallelAndStreamsHarnessSteps(t *testing.T) {
	installAutomaticPreviewQAMediaFixture(t)
	database, service, draftID, _, timelineID := prepareAutomaticPreviewQAFixture(t, nil)
	t.Cleanup(service.Close)

	before, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	ctx = withTerminalTimelineTruthState(ctx, newTerminalTimelineTruthState())
	report := service.executeAutomaticPreviewQA(ctx, draftID, "explicit_preview_or_qa_request", false)
	if report.Status != "succeeded" || report.TimelineID != timelineID ||
		len(report.CoreChecks) != len(automaticPreviewCoreChecks) ||
		report.VisualAdvisory != nil {
		t.Fatalf("report=%#v", report)
	}
	checks := map[string]bool{}
	for _, result := range report.CoreChecks {
		checks[result.Check] = true
	}
	for _, name := range automaticPreviewCoreChecks {
		if !checks[name] {
			t.Fatalf("缺少 core check %q: %#v", name, report.CoreChecks)
		}
	}
	if entrants := automaticPreviewQABarrierEntrants(t); entrants != 5 {
		t.Fatalf("五项检查未并发越过真实 ffprobe 屏障: entrants=%d", entrants)
	}
	after, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || after.TimelineID != before.TimelineID {
		t.Fatalf("Preview QA 改写了时间线: before=%s after=%#v err=%v", before.TimelineID, after, err)
	}

	events, _, unsubscribe := service.Hub().Subscribe(draftID)
	unsubscribe()
	assertAutomaticPreviewQASteps(t, events, map[string]int{
		"preview.generate":  1,
		"preview.check":     5,
		"preview.qa_report": 1,
	})

	for _, name := range []string{"asset.list_assets", "preview.generate", "preview.check"} {
		spec, exists := service.Tools().Spec(name)
		if !exists || spec.Exposure != rushestools.ExposureHarness {
			t.Fatalf("%s spec=%#v exists=%v", name, spec, exists)
		}
		for _, modelTool := range service.Tools().EinoTools(true, false) {
			info, infoErr := modelTool.Info(t.Context())
			if infoErr != nil {
				t.Fatal(infoErr)
			}
			if info.Name == name {
				t.Fatalf("Harness-only %s 泄漏进模型目录", name)
			}
		}
	}
}

func TestAutomaticPreviewQAVisualIsAdvisoryAndExactVersionRunsOnce(t *testing.T) {
	installAutomaticPreviewQAMediaFixture(t)
	database, service, draftID, _, timelineID := prepareAutomaticPreviewQAFixture(t, nil)
	t.Cleanup(service.Close)

	state := newAutomaticPreviewQAState()
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	ctx = withAutomaticPreviewQAState(ctx, state)
	ctx = withTerminalTimelineTruthState(ctx, newTerminalTimelineTruthState())
	messages := []*schema.Message{schema.UserMessage("请生成预览并检查画面构图")}
	candidate := schema.AssistantMessage("已完成，可交付。", nil)
	first, err := service.shouldRunAutomaticPreviewQA(ctx, messages, candidate)
	if err != nil || !first {
		t.Fatalf("first=%v err=%v", first, err)
	}
	second, err := service.shouldRunAutomaticPreviewQA(ctx, messages, candidate)
	if err != nil || second {
		t.Fatalf("同一 timeline_id=%s 不得重复验收: second=%v err=%v", timelineID, second, err)
	}
	if !state.claim(draftID + ":v2") {
		t.Fatal("模型修复产生新 timeline_id 后应允许重新进入 Preview QA")
	}

	report := service.executeAutomaticPreviewQA(ctx, draftID, "explicit_preview_or_qa_request", true)
	if report.Status != "succeeded" || !report.Passed || report.VisualAdvisory == nil ||
		report.VisualAdvisory.Check != "visual" {
		t.Fatalf("visual advisory report=%#v", report)
	}
	if !report.Degraded {
		t.Fatal("未配置视觉模型时 visual advisory 应明确 degraded，而不是伪装完成")
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.TimelineID != timelineID {
		t.Fatalf("visual advisory 改写时间线: latest=%#v err=%v", latest, err)
	}
}

type automaticPreviewQAReactModel struct {
	mu         sync.Mutex
	calls      int
	bound      []string
	sawReport  bool
	userCounts []int
}

func (stub *automaticPreviewQAReactModel) WithTools(
	infos []*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.bound = stub.bound[:0]
	for _, info := range infos {
		stub.bound = append(stub.bound, info.Name)
	}
	return stub, nil
}

func (stub *automaticPreviewQAReactModel) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.calls++
	for _, internal := range []string{"asset.list_assets", "preview.generate", "preview.check"} {
		if containsName(stub.bound, internal) {
			return nil, fmt.Errorf("Harness-only %s 泄漏给模型: %v", internal, stub.bound)
		}
	}
	users := 0
	for _, message := range messages {
		if message == nil {
			continue
		}
		if message.Role == schema.User {
			users++
		}
		if message.Role == schema.System &&
			message.Extra["context_phase"] == automaticPreviewQAContextPhase &&
			strings.Contains(message.Content, "PreviewQAReport") {
			stub.sawReport = true
		}
	}
	stub.userCounts = append(stub.userCounts, users)
	switch stub.calls {
	case 1:
		return schema.AssistantMessage("已完成，可交付。", nil), nil
	case 2:
		if !stub.sawReport {
			return nil, errors.New("Harness 报告未作为 system evidence 回灌同一 ReAct transcript")
		}
		return schema.AssistantMessage("Preview QA 已通过，工作预览已就绪；最终导出请在 UI 触发。", nil), nil
	default:
		return nil, fmt.Errorf("unexpected provider call %d", stub.calls)
	}
}

func (stub *automaticPreviewQAReactModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := stub.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestAutomaticPreviewQAReportReturnsToModelWithoutSyntheticUserMessage(t *testing.T) {
	installAutomaticPreviewQAMediaFixture(t)
	provider := &automaticPreviewQAReactModel{}
	_, service, draftID, _, _ := prepareAutomaticPreviewQAFixture(t, provider)
	t.Cleanup(service.Close)

	ctx := rushestools.WithDraftID(t.Context(), draftID)
	ctx = withAutomaticPreviewQAState(ctx, newAutomaticPreviewQAState())
	ctx = withTerminalTimelineTruthState(ctx, newTerminalTimelineTruthState())
	ctx = withToolRecoveryState(ctx, newToolRecoveryState())
	ctx = withTurnBudgetState(ctx, newTurnBudgetState(maxToolRoundsPerTurn))
	ctx = withTestTurnLeaseSession(t, service, ctx, draftID)
	ctx = withModelToolSurfaceSession(ctx)
	ctx = agentexec.WithTurnInteractionState(
		ctx, agentexec.NewTurnInteractionState(service.indexedResources),
	)
	ctx = rushestools.WithReporter(ctx, service.toolReporter(ctx, draftID))
	response, err := service.react.Generate(ctx, []*schema.Message{
		schema.UserMessage("请生成预览并检查黑帧。"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || !strings.Contains(response.Content, "最终导出请在 UI 触发") {
		t.Fatalf("response=%#v", response)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.calls != 2 || !provider.sawReport ||
		len(provider.userCounts) != 2 || provider.userCounts[0] != 1 || provider.userCounts[1] != 1 {
		t.Fatalf(
			"calls=%d saw_report=%v user_counts=%v",
			provider.calls, provider.sawReport, provider.userCounts,
		)
	}
}

func TestAutomaticPreviewQARenderFailureLeavesTimelineUnchanged(t *testing.T) {
	installAutomaticPreviewQAMediaFixture(t)
	database, service, draftID, _, timelineID := prepareAutomaticPreviewQAFixtureWithoutJob(t)
	t.Cleanup(service.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	ctx = rushestools.WithDraftID(ctx, draftID)
	ctx = withTerminalTimelineTruthState(ctx, newTerminalTimelineTruthState())

	report := service.executeAutomaticPreviewQA(ctx, draftID, "explicit_preview_or_qa_request", false)
	if report.Status != "render_failed" || len(report.Errors) == 0 {
		t.Fatalf("report=%#v", report)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.TimelineID != timelineID {
		t.Fatalf("render failure 改写时间线: latest=%#v err=%v", latest, err)
	}
}

func TestAutomaticPreviewQATriggerAndClaimBoundaries(t *testing.T) {
	base := withTerminalTimelineTruthState(t.Context(), newTerminalTimelineTruthState())
	completed := schema.AssistantMessage("已完成，可交付。", nil)
	if got := automaticPreviewQATrigger(base, []*schema.Message{
		schema.UserMessage("请生成预览。"),
	}, completed); got != "explicit_preview_or_qa_request" {
		t.Fatalf("explicit preview trigger=%q", got)
	}
	if got := automaticPreviewQATrigger(base, []*schema.Message{
		schema.UserMessage("请质检这段代码。"),
	}, completed); got != "" {
		t.Fatalf("非媒体质检不应触发 Preview QA: %q", got)
	}

	truth := terminalTimelineTruthFromContext(base)
	truth.recordMutationTimelineID("draft_trigger:v1")
	playbook := schema.SystemMessage("talking-head playbook")
	playbook.Extra = map[string]any{"preview_qa_required": true}
	if got := automaticPreviewQATrigger(base, []*schema.Message{
		playbook, schema.UserMessage("完成剪辑。"),
	}, completed); got != "playbook_required" {
		t.Fatalf("playbook trigger=%q", got)
	}
	if got := automaticPreviewQATrigger(base, []*schema.Message{
		schema.UserMessage("完成剪辑。"),
	}, completed); got != "deliverable_declaration" {
		t.Fatalf("deliverable trigger=%q", got)
	}
	for _, candidate := range []*schema.Message{
		nil,
		schema.AssistantMessage("", nil),
		schema.AssistantMessage("尚未完成，仍需处理。", nil),
	} {
		if got := automaticPreviewQATrigger(base, []*schema.Message{
			schema.UserMessage("完成剪辑。"),
		}, candidate); got != "" {
			t.Fatalf("非终态 candidate=%#v trigger=%q", candidate, got)
		}
	}

	var nilState *automaticPreviewQAState
	if nilState.claim("draft:v1") {
		t.Fatal("nil state 不得认领 Preview QA")
	}
	state := newAutomaticPreviewQAState()
	if state.claim("") {
		t.Fatal("空 timeline_id 不得认领 Preview QA")
	}
	for version := 1; version <= maxAutomaticPreviewQAPassesPerTurn; version++ {
		if !state.claim(fmt.Sprintf("draft:v%d", version)) {
			t.Fatalf("第 %d 个精确版本应可认领", version)
		}
	}
	if state.claim("draft:v1") || state.claim("draft:v5") {
		t.Fatal("同版本重复或超过单回合上限不得再次验收")
	}
	if got := previewQAEvidenceJSON(make(chan int)); got != "" {
		t.Fatalf("不可序列化证据应为空: %q", got)
	}
}

func TestAutomaticPreviewQAMissingTimelineReturnsSystemEvidenceOnce(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	draftID := "draft_preview_qa_missing_" + agentexec.RandomID("case")
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	state := newAutomaticPreviewQAState()
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	ctx = withAutomaticPreviewQAState(ctx, state)
	ctx = withTerminalTimelineTruthState(ctx, newTerminalTimelineTruthState())
	messages := []*schema.Message{schema.UserMessage("请生成视频预览并质检。")}
	candidate := schema.AssistantMessage("已完成。", nil)
	shouldRun, err := service.shouldRunAutomaticPreviewQA(ctx, messages, candidate)
	if err != nil || !shouldRun {
		t.Fatalf("missing timeline should_run=%v err=%v", shouldRun, err)
	}
	shouldRun, err = service.shouldRunAutomaticPreviewQA(ctx, messages, candidate)
	if err != nil || shouldRun {
		t.Fatalf("missing timeline 同一 turn 只能报告一次: should_run=%v err=%v", shouldRun, err)
	}

	evidence, err := service.runAutomaticPreviewQA(ctx, messages, candidate)
	if err != nil || evidence == nil || evidence.Role != schema.System ||
		evidence.Extra["context_phase"] != automaticPreviewQAContextPhase {
		t.Fatalf("evidence=%#v err=%v", evidence, err)
	}
	report := decodePreviewQAReport(t, evidence)
	if report.Status != "not_available" || report.Passed ||
		len(report.Errors) != 1 || report.Errors[0]["error_code"] !=
		string(rushestools.ErrCodePreviewQATimelineMissing) {
		t.Fatalf("report=%#v", report)
	}

	noState, err := service.shouldRunAutomaticPreviewQA(
		rushestools.WithDraftID(t.Context(), draftID), messages, candidate,
	)
	if err != nil || noState {
		t.Fatalf("无自动状态不应运行: should_run=%v err=%v", noState, err)
	}
	noTrigger, err := service.shouldRunAutomaticPreviewQA(
		withAutomaticPreviewQAState(
			rushestools.WithDraftID(t.Context(), draftID), newAutomaticPreviewQAState(),
		),
		[]*schema.Message{schema.UserMessage("继续。")}, candidate,
	)
	if err != nil || noTrigger {
		t.Fatalf("无触发边界不应运行: should_run=%v err=%v", noTrigger, err)
	}
	if _, err := service.shouldRunAutomaticPreviewQA(
		withAutomaticPreviewQAState(t.Context(), newAutomaticPreviewQAState()),
		messages, candidate,
	); err == nil {
		t.Fatal("缺少 draft_id 应返回错误")
	}
}

func TestAutomaticPreviewQACoreExecutionFailureIsBlocking(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("RUSHES_FFMPEG_SANDBOX", "0")
	database, service, draftID, _, timelineID := prepareAutomaticPreviewQAFixture(t, nil)
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	ctx = withTerminalTimelineTruthState(ctx, newTerminalTimelineTruthState())

	report := service.executeAutomaticPreviewQA(ctx, draftID, "explicit_preview_or_qa_request", false)
	if report.Status != "check_failed" || report.Passed ||
		len(report.CoreChecks) != 0 || len(report.Errors) != len(automaticPreviewCoreChecks) {
		t.Fatalf("report=%#v", report)
	}
	for _, failure := range report.Errors {
		if failure["error_code"] != string(rushestools.ErrCodePreviewQACheck) ||
			failure["check"] == "" || failure["message"] == "" {
			t.Fatalf("failure=%#v", failure)
		}
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.TimelineID != timelineID {
		t.Fatalf("core check failure 改写时间线: latest=%#v err=%v", latest, err)
	}
}

func TestAutomaticPreviewQAStopsWhenExactTimelineValidationFails(t *testing.T) {
	database, service, draftID, _, _ := prepareAutomaticPreviewQAFixture(t, nil)
	t.Cleanup(service.Close)
	document, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	document.Version = 2
	document.TimelineID = draftID + ":v2"
	for index := range document.Tracks {
		if document.Tracks[index].TrackID != "bgm" {
			continue
		}
		document.Tracks[index].Clips = []timeline.Clip{{
			TimelineClipID: "bgm_overhang", TrackID: "bgm", AssetID: "music",
			AssetKind: "audio", TimelineEndFrame: 90, SourceEndFrame: 90, PlaybackRate: 1,
		}}
	}
	seeded, err := seedTimelineVersion(
		service, t.Context(), draftID, document, "invalid_preview_qa_fixture", nil,
	)
	if err != nil || seeded.Status != string(rushestools.StatusValidationFailed) {
		t.Fatalf("seeded=%#v err=%v", seeded, err)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	ctx = withTerminalTimelineTruthState(ctx, newTerminalTimelineTruthState())
	report := service.executeAutomaticPreviewQA(ctx, draftID, "deliverable_declaration", false)
	if report.Status != "validation_failed" || report.Passed || report.PreviewID != "" ||
		report.TimelineCheck.Status != string(rushestools.StatusValidationFailed) {
		t.Fatalf("report=%#v", report)
	}
}

func TestAutomaticPreviewQACoreSignalErrorBlocksPass(t *testing.T) {
	installAutomaticPreviewQAMediaFixture(t)
	fakeBin := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	ffmpeg := `#!/bin/sh
set -eu
case "$*" in
  *"-v error"*) exit 1 ;;
  *blackdetect*) printf 'black_start:0 black_end:0.5 black_duration:0.5\n' >&2 ;;
  *freezedetect*) printf 'freeze_start: 1.0\nfreeze_end: 1.7 | freeze_duration: 0.7\n' >&2 ;;
  *silencedetect*) printf 'silence_start: 0\nsilence_end: 0.6 | silence_duration: 0.6\n' >&2 ;;
  *ebur128*) printf 'Integrated loudness:\n I: -16.0 LUFS\n' >&2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(fakeBin, "ffmpeg"), []byte(ffmpeg), 0o755); err != nil {
		t.Fatal(err)
	}
	_, service, draftID, _, _ := prepareAutomaticPreviewQAFixture(t, nil)
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	ctx = withTerminalTimelineTruthState(ctx, newTerminalTimelineTruthState())
	report := service.executeAutomaticPreviewQA(ctx, draftID, "explicit_preview_or_qa_request", false)
	if report.Status != "succeeded" || report.Passed ||
		len(report.CoreChecks) != len(automaticPreviewCoreChecks) ||
		!previewQAHasErrorIssue(report.Issues) || !strings.Contains(report.Summary, "阻断错误") {
		t.Fatalf("report=%#v", report)
	}
}

func TestAutomaticPreviewQAVisualExecutionFailureStaysAdvisory(t *testing.T) {
	installAutomaticPreviewQAMediaFixture(t)
	fakeBin := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	barrier := os.Getenv("RUSHES_PREVIEW_QA_BARRIER")
	ffprobe := fmt.Sprintf(`#!/bin/sh
set -eu
barrier=%q
while ! mkdir "$barrier/count.lock" 2>/dev/null; do
  sleep 0.01
done
count=0
if [ -f "$barrier/count" ]; then
  count="$(cat "$barrier/count")"
fi
count="$((count + 1))"
printf '%%s' "$count" > "$barrier/count"
rmdir "$barrier/count.lock"
if [ "$count" -gt 10 ]; then
  exit 1
fi
cat <<'JSON'
{"format":{"duration":"2.0"},"streams":[{"codec_type":"video","duration":"2.0","avg_frame_rate":"30/1","width":320,"height":240},{"codec_type":"audio","duration":"2.0"}]}
JSON
`, barrier)
	if err := os.WriteFile(filepath.Join(fakeBin, "ffprobe"), []byte(ffprobe), 0o755); err != nil {
		t.Fatal(err)
	}
	_, service, draftID, _, _ := prepareAutomaticPreviewQAFixture(t, nil)
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	ctx = withTerminalTimelineTruthState(ctx, newTerminalTimelineTruthState())
	report := service.executeAutomaticPreviewQA(ctx, draftID, "explicit_preview_or_qa_request", true)
	if report.Status != "succeeded" || !report.Passed || !report.Degraded ||
		report.VisualAdvisory != nil {
		t.Fatalf("report=%#v", report)
	}
	found := false
	for _, issue := range report.Issues {
		if issue["error_code"] == string(rushestools.ErrCodePreviewQAVisual) &&
			issue["severity"] == "warning" {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺少 visual advisory 失败证据: %#v", report.Issues)
	}
}

func assertAutomaticPreviewQASteps(
	t *testing.T,
	events []StreamEvent,
	wantFinished map[string]int,
) {
	t.Helper()
	started := map[string]int{}
	progress := map[string]int{}
	finished := map[string]int{}
	for _, event := range events {
		name := agentexec.InterfaceString(event["tool"])
		if _, tracked := wantFinished[name]; !tracked {
			continue
		}
		if event["harness_owned"] != true {
			t.Fatalf("step 未标记 Harness-owned: %#v", event)
		}
		switch event["type"] {
		case TurnStreamToolStepStarted:
			started[name]++
		case TurnStreamToolStepProgress:
			if event["progress"] == 0.5 {
				progress[name]++
			}
		case TurnStreamToolStepFinished:
			if event["progress"] != 1 || event["duration_ms"] == nil {
				t.Fatalf("终态 step 缺少进度或耗时: %#v", event)
			}
			finished[name]++
		}
	}
	for name, want := range wantFinished {
		if started[name] != want || progress[name] != want || finished[name] != want {
			t.Fatalf(
				"%s lifecycle started=%d progress=%d finished=%d want=%d",
				name, started[name], progress[name], finished[name], want,
			)
		}
	}
}

func prepareAutomaticPreviewQAFixture(
	t *testing.T,
	provider model.ToolCallingChatModel,
) (*storage.DB, *Service, string, string, string) {
	t.Helper()
	database, service, draftID, _, timelineID := prepareAutomaticPreviewQAFixtureWithoutJobWithProvider(t, provider)
	const previewID = "preview_auto_qa"
	source := filepath.Join(database.Paths.Temporary, "automatic-preview-qa-source.mp4")
	seedAutomaticPreviewQAArtifact(t, database, draftID, previewID, source)
	seedAutomaticPreviewQACompletedJob(t, database, draftID, previewID)
	return database, service, draftID, previewID, timelineID
}

func prepareAutomaticPreviewQAFixtureWithoutJob(
	t *testing.T,
) (*storage.DB, *Service, string, string, string) {
	t.Helper()
	return prepareAutomaticPreviewQAFixtureWithoutJobWithProvider(t, nil)
}

func prepareAutomaticPreviewQAFixtureWithoutJobWithProvider(
	t *testing.T,
	provider model.ToolCallingChatModel,
) (*storage.DB, *Service, string, string, string) {
	t.Helper()
	database := agenttest.AgentTestDatabase(t)
	draftID := "draft_auto_preview_qa_" + agentexec.RandomID("case")
	assetID := "asset_auto_preview_qa_" + agentexec.RandomID("case")
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, provider)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(database.Paths.Temporary, "automatic-preview-qa-source.mp4")
	if err := os.WriteFile(source, []byte("deterministic automatic preview fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedAutomaticPreviewQAAsset(t, database, draftID, assetID, source)
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: assetID, AssetKind: "video", HasAudio: true,
		SourceStartFrame: 0, SourceEndFrame: 60, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if persisted, persistErr := seedTimelineVersion(
		service, t.Context(), draftID, document, "automatic_preview_qa_fixture", nil,
	); persistErr != nil || persisted.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("persist timeline=%#v err=%v", persisted, persistErr)
	}
	return database, service, draftID, assetID, document.TimelineID
}

func seedAutomaticPreviewQAAsset(
	t *testing.T,
	database *storage.DB,
	draftID, assetID, source string,
) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{
			Type: "AssetImported",
			Payload: map[string]any{
				"asset_id": assetID, "job_id": "job_" + assetID,
				"storage_mode": "reference", "reference_path": source,
				"kind": "video", "source": "local_path", "filename": filepath.Base(source),
				"hash": assetID, "size": 39, "ingest_status": "ready", "usable": true,
				"probe": map[string]any{"duration_sec": 2.0, "has_audio": true},
			},
		},
		{
			Type: "AssetLinked", DraftID: draftID,
			Payload: map[string]any{"asset_id": assetID},
		},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed asset status=%s err=%v", result.Status, err)
	}
}

func seedAutomaticPreviewQAArtifact(
	t *testing.T,
	database *storage.DB,
	draftID, previewID, source string,
) {
	t.Helper()
	object, err := media.NewObjectStore(database.Paths).PutFile(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "PreviewRendered", DraftID: draftID,
		Payload: map[string]any{
			"artifact_id": previewID, "timeline_version": 1,
			"object_hash": object.Hash, "object_size": object.Size,
			"render_width": 320, "render_height": 240,
			"render_fps": 30, "expected_duration_sec": 2,
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed preview status=%s err=%v", result.Status, err)
	}
}

func seedAutomaticPreviewQACompletedJob(
	t *testing.T,
	database *storage.DB,
	draftID, previewID string,
) {
	t.Helper()
	const jobID = "job_auto_preview_qa"
	now := time.Now().UTC()
	enqueued, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: draftID,
		Payload: map[string]any{
			"job_id": jobID, "kind": "render_preview", "requested_by_draft_id": draftID,
			"idempotency_key": "render_preview:" + draftID + ":1:auto",
			"job_payload":     map[string]any{"timeline_version": 1, "orientation": "auto"},
			"next_run_at":     now.Format(time.RFC3339Nano), "max_retries": 2,
		},
	}}, reducer.Options{Actor: contracts.ActorAgent})
	if err != nil || enqueued.Status != reducer.StatusApplied {
		t.Fatalf("enqueue preview status=%s err=%v", enqueued.Status, err)
	}
	completed, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobSucceeded", DraftID: draftID,
		Payload: map[string]any{
			"job_id": jobID, "kind": "render_preview", "requested_by_draft_id": draftID,
			"result": map[string]any{
				"artifact_id": previewID, "timeline_version": 1, "orientation": "auto",
			},
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || completed.Status != reducer.StatusApplied {
		t.Fatalf("complete preview status=%s err=%v", completed.Status, err)
	}
}

func installAutomaticPreviewQAMediaFixture(t *testing.T) {
	t.Helper()
	fakeBin := t.TempDir()
	barrier := t.TempDir()
	ffprobe := fmt.Sprintf(`#!/bin/sh
set -eu
barrier=%q
if [ ! -f "$barrier/released" ]; then
  : > "$barrier/entered.$$"
  while [ "$(ls "$barrier"/entered.* 2>/dev/null | wc -l | tr -d ' ')" -lt 5 ]; do
    sleep 0.01
  done
  : > "$barrier/released"
fi
cat <<'JSON'
{"format":{"duration":"2.0"},"streams":[{"codec_type":"video","duration":"2.0","avg_frame_rate":"30/1","width":320,"height":240},{"codec_type":"audio","duration":"2.0"}]}
JSON
`, barrier)
	ffmpeg := `#!/bin/sh
set -eu
case "$*" in
  *blackdetect*) printf 'black_start:0 black_end:0.5 black_duration:0.5\n' >&2 ;;
  *freezedetect*) printf 'freeze_start: 1.0\nfreeze_end: 1.7 | freeze_duration: 0.7\n' >&2 ;;
  *silencedetect*) printf 'silence_start: 0\nsilence_end: 0.6 | silence_duration: 0.6\n' >&2 ;;
  *ebur128*) printf 'Integrated loudness:\n I: -16.0 LUFS\n' >&2 ;;
esac
`
	for name, body := range map[string]string{"ffprobe": ffprobe, "ffmpeg": ffmpeg} {
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RUSHES_FFMPEG_SANDBOX", "0")
	t.Setenv("RUSHES_PREVIEW_QA_BARRIER", barrier)
}

func automaticPreviewQABarrierEntrants(t *testing.T) int {
	t.Helper()
	entrants, err := filepath.Glob(filepath.Join(
		os.Getenv("RUSHES_PREVIEW_QA_BARRIER"), "entered.*",
	))
	if err != nil {
		t.Fatal(err)
	}
	return len(entrants)
}

func decodePreviewQAReport(t *testing.T, message *schema.Message) PreviewQAReport {
	t.Helper()
	start := strings.IndexByte(message.Content, '{')
	end := strings.LastIndexByte(message.Content, '}')
	if start < 0 || end <= start {
		t.Fatalf("invalid PreviewQAReport message: %q", message.Content)
	}
	var report PreviewQAReport
	if err := json.Unmarshal([]byte(message.Content[start:end+1]), &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func dynamicPreviewQACurrentView(messages []*schema.Message) (map[string]any, error) {
	var current *schema.Message
	for _, message := range messages {
		if message == nil || message.Extra["context_phase"] != currentTimelineViewContextPhase {
			continue
		}
		if current != nil {
			return nil, errors.New("provider 同时收到多份 CurrentTimelineView")
		}
		current = message
	}
	if current == nil {
		return nil, errors.New("provider 缺少 CurrentTimelineView")
	}
	start := strings.IndexByte(current.Content, '{')
	end := strings.LastIndex(current.Content, "\n这是当前时间线")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("CurrentTimelineView 格式无效: %s", current.Content)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(current.Content[start:end]), &view); err != nil {
		return nil, err
	}
	return view, nil
}
