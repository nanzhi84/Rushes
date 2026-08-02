package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type blankMutationCallIDModel struct {
	mu          sync.Mutex
	calls       int
	insertBound bool
	sawFailure  bool
}

func (value *blankMutationCallIDModel) WithTools(
	infos []*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	value.mu.Lock()
	defer value.mu.Unlock()
	for _, info := range infos {
		if info.Name == "timeline.insert" {
			value.insertBound = true
		}
	}
	return value, nil
}

func (value *blankMutationCallIDModel) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	value.mu.Lock()
	defer value.mu.Unlock()
	value.calls++
	if value.calls == 1 {
		if !value.insertBound {
			return nil, errors.New("timeline.insert 未绑定")
		}
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "",
			Function: schema.FunctionCall{
				Name:      "timeline.insert",
				Arguments: `{"kind":"insert_subtitle","start_frame":0,"end_frame":30,"text":"不得提交"}`,
			},
		}}), nil
	}
	if len(messages) == 0 || messages[len(messages)-1].Role != schema.Tool ||
		!strings.Contains(messages[len(messages)-1].Content, `"status":"failed"`) ||
		!strings.Contains(messages[len(messages)-1].Content, "调用身份") {
		return nil, errors.New("空 tool_call_id 没有在执行前回灌结构化失败")
	}
	value.sawFailure = true
	return schema.AssistantMessage("这次工具调用身份无效，时间线没有被修改。", nil), nil
}

func (value *blankMutationCallIDModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := value.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (value *blankMutationCallIDModel) snapshot() (int, bool) {
	value.mu.Lock()
	defer value.mu.Unlock()
	return value.calls, value.sawFailure
}

func TestTimelineReceiptReplayPreservesCompleteTerminalResult(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_receipt_equivalent"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	ctx := withTestTurnLeaseSession(t, service, t.Context(), draftID)

	execute := func(callID, name string, input any) rushestools.ToolResult {
		t.Helper()
		raw, executeErr := service.ExecuteTool(
			rushestools.WithToolCallID(ctx, callID), name, input,
		)
		if executeErr != nil {
			t.Fatalf("%s: %v", name, executeErr)
		}
		result := raw.(rushestools.ToolResult)
		if result.Status != string(rushestools.StatusSucceeded) {
			t.Fatalf("%s result=%#v", name, result)
		}
		return result
	}
	execute("call_insert_first", "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "asset_surface",
		"source_start_frame": 0, "source_end_frame": 60,
	})
	execute("call_insert_second", "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "asset_surface",
		"source_start_frame": 60, "source_end_frame": 120,
	})
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	firstClipID := latest.Tracks[0].Clips[0].TimelineClipID
	trimInput := rushestools.TimelineUpdateInput{
		"kind": "trim_clip", "timeline_clip_id": firstClipID,
		"source_start_frame": 0, "source_end_frame": 30,
	}
	first := execute("call_trim_once", "timeline.update", trimInput)
	reused := execute("call_trim_once", "timeline.update", trimInput)
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	reusedJSON, err := json.Marshal(reused)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(reusedJSON) {
		t.Fatalf("receipt replay payload drift\nfirst=%s\nreused=%s", firstJSON, reusedJSON)
	}
	for _, key := range []string{
		"applied_operation", "changed_targets", "coordinate_effect", "validation_summary",
	} {
		if reused.Data[key] == nil {
			t.Fatalf("receipt replay missing %s: %#v", key, reused.Data)
		}
	}
	coordinateEffect, _ := reused.Data["coordinate_effect"].(map[string]any)
	if coordinateEffect["observation_required"] != true {
		t.Fatalf("receipt replay did not retain coordinate effect: %s", reusedJSON)
	}
	var versions int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?`, draftID,
	).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 3 {
		t.Fatalf("receipt replay created duplicate timeline: versions=%d want=3", versions)
	}
}

func TestCommittedTimelineReceiptPreventsCrashReplay(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const (
		draftID       = "draft_receipt_crash"
		turnID        = "turn_receipt_crash"
		sourceMessage = "message_receipt_crash"
		toolCallID    = "call_receipt_crash"
	)
	agenttest.CreateAgentDraft(t, database, draftID)
	input := rushestools.TimelineInsertInput{
		"kind": "insert_subtitle", "start_frame": 0, "end_frame": 30, "text": "只提交一次",
	}
	fingerprint, err := canonicalToolInputFingerprint("timeline.insert", input)
	if err != nil {
		t.Fatal(err)
	}
	start, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{AgentTurnRunStart: &reducer.AgentTurnRunStartRow{
			TurnID: turnID, DraftID: draftID, SourceItemID: sourceMessage,
			Kind: string(QueueUserMessage),
		}},
	})
	if err != nil || start.Status != reducer.StatusApplied {
		t.Fatalf("start=%#v err=%v", start, err)
	}
	commitContext, cancelCommit := context.WithCancelCause(t.Context())
	commitSession := newTimelineEditLeaseSession(
		database, draftID, turnID, cancelCommit,
	)
	commitContext = rushestools.WithDraftID(commitContext, draftID)
	commitContext = rushestools.WithTurnIdentity(commitContext, turnID, sourceMessage)
	commitContext = withTimelineEditLeaseSession(commitContext, commitSession)
	commitContext = rushestools.WithTimelineWriteAdmission(
		commitContext, turnID, commitSession.token, commitSession.markLost,
	)
	if err := commitSession.ensure(commitContext); err != nil {
		t.Fatal(err)
	}
	document := timeline.Empty(draftID, 1)
	documentMap, err := timeline.ToMap(document)
	if err != nil {
		t.Fatal(err)
	}
	terminal := rushestools.ToolResult{
		Status: "succeeded", Observation: "时间线修改已提交",
		Data: map[string]any{"timeline_id": document.TimelineID, "timeline_version": 1},
	}
	terminalJSON, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	baseVersion := 0
	committed, err := reducer.Apply(commitContext, database, []contracts.Event{{
		Type: "TimelineVersionCreated", DraftID: draftID,
		Payload: map[string]any{
			"timeline_id": document.TimelineID, "timeline_version": 1,
			"patch_id": "patch_receipt_crash", "document_json": documentMap,
			"edit_origin": "agent", "edit_operations": []map[string]any{input},
		},
	}}, reducer.Options{
		Actor: contracts.ActorAgent, BaseVersion: &baseVersion,
		TimelineWriteAdmission: &reducer.TimelineWriteAdmission{
			Origin: "agent", TurnID: turnID, LeaseToken: commitSession.token,
		},
		ResultRows: reducer.ResultRows{AgentToolReceipt: &reducer.AgentToolReceiptRow{
			InvocationKey: "receipt_crash", DraftID: draftID,
			SourceMessageID: sourceMessage, TurnID: turnID, ToolCallID: toolCallID,
			ToolName: "timeline.insert", ArgumentFingerprint: fingerprint,
			BeforeVersion: 0, AfterTimelineID: document.TimelineID, AfterVersion: 1,
			TerminalStatus: terminal.Status, ResultJSON: string(terminalJSON),
		}},
	})
	if err != nil || committed.Status != reducer.StatusApplied {
		t.Fatalf("commit=%#v err=%v", committed, err)
	}
	commitSession.close()
	cancelCommit(nil)

	// NewService simulates restart: it marks the in-flight turn interrupted, but
	// the exact invocation receipt remains reusable and the mutation is not run.
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	ctx, cancelReplay := context.WithCancelCause(t.Context())
	replaySession := newTimelineEditLeaseSession(
		database, draftID, turnID, cancelReplay,
	)
	t.Cleanup(func() {
		replaySession.close()
		cancelReplay(nil)
	})
	ctx = rushestools.WithDraftID(ctx, draftID)
	ctx = rushestools.WithTurnIdentity(ctx, turnID, sourceMessage)
	ctx = rushestools.WithToolCallID(ctx, toolCallID)
	ctx = withTimelineEditLeaseSession(ctx, replaySession)
	ctx = rushestools.WithTimelineWriteAdmission(
		ctx, turnID, replaySession.token, replaySession.markLost,
	)
	raw, err := service.ExecuteTool(ctx, "timeline.insert", input)
	if err != nil {
		t.Fatal(err)
	}
	reused := raw.(rushestools.ToolResult)
	if reused.Status != terminal.Status || reused.Data["timeline_id"] != document.TimelineID {
		t.Fatalf("reused=%#v", reused)
	}
	var versions int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?`, draftID,
	).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatalf("receipt replay created duplicate timeline versions=%d", versions)
	}
	changedInput := rushestools.TimelineInsertInput{
		"kind": "insert_subtitle", "start_frame": 0, "end_frame": 30, "text": "不同参数",
	}
	if _, err := service.ExecuteTool(ctx, "timeline.insert", changedInput); err == nil {
		t.Fatal("相同 invocation key 携带不同参数必须 fail closed")
	}
	run, err := storage.GetAgentTurnRunBySource(
		t.Context(), database.Read(), draftID, sourceMessage, string(QueueUserMessage),
	)
	if err != nil || run.Status != "interrupted" {
		t.Fatalf("run=%#v err=%v", run, err)
	}
}

func TestModelMutationWithoutToolCallIDCannotCommitTimelineOrReceipt(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const (
		draftID   = "draft_blank_mutation_call_id"
		messageID = "message_blank_mutation_call_id"
	)
	agenttest.CreateAgentDraft(t, database, draftID)
	agenttest.InsertAgentMessage(t, database, draftID, messageID, "在时间线添加一条字幕")
	provider := &blankMutationCallIDModel{}
	service, err := NewService(t.Context(), database, provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if !service.Queue().EnqueueUserMessage(draftID, messageID, "在时间线添加一条字幕") {
		t.Fatal("enqueue failed")
	}
	service.Queue().JoinDraft(draftID)
	if calls, sawFailure := provider.snapshot(); calls != 2 || !sawFailure {
		t.Fatalf("provider calls=%d saw_failure=%t", calls, sawFailure)
	}
	var versions, receipts int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT
			(SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?),
			(SELECT COUNT(*) FROM agent_tool_receipts WHERE draft_id=?)`,
		draftID, draftID,
	).Scan(&versions, &receipts); err != nil {
		t.Fatal(err)
	}
	if versions != 0 || receipts != 0 {
		t.Fatalf("blank call id leaked durable state: versions=%d receipts=%d", versions, receipts)
	}
	if _, err := storage.GetLiveAgentEditLease(
		t.Context(), database.Read(), draftID, time.Now().UTC(),
	); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("turn leaked edit lease: %v", err)
	}
}

func TestTimelineReceiptCorruptionAndLookupFailuresFailClosed(t *testing.T) {
	if _, err := canonicalToolInputFingerprint("timeline.update", func() {}); err == nil {
		t.Fatal("unserializable input fingerprint unexpectedly succeeded")
	}

	database := agenttest.AgentTestDatabase(t)
	const (
		draftID       = "draft_receipt_corruption"
		turnID        = "turn_receipt_corruption"
		sourceMessage = "message_receipt_corruption"
		toolCallID    = "call_receipt_corruption"
	)
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	ctx = rushestools.WithTurnIdentity(ctx, turnID, sourceMessage)
	ctx = rushestools.WithToolCallID(ctx, toolCallID)
	if _, _, _, err := service.prepareTimelineMutationReceipt(
		ctx, "timeline.update", func() {},
	); err == nil {
		t.Fatal("prepare accepted an unserializable mutation input")
	}

	input := rushestools.TimelineUpdateInput{
		"kind": "adjust_gain", "timeline_clip_id": "clip-1", "gain_db": -6,
	}
	fingerprint, err := canonicalToolInputFingerprint("timeline.update", input)
	if err != nil {
		t.Fatal(err)
	}
	start, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{AgentTurnRunStart: &reducer.AgentTurnRunStartRow{
			TurnID: turnID, DraftID: draftID, SourceItemID: sourceMessage,
			Kind: string(QueueUserMessage),
		}},
	})
	if err != nil || start.Status != reducer.StatusApplied {
		t.Fatalf("start=%#v err=%v", start, err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO agent_tool_receipts(
			invocation_key,draft_id,source_message_id,turn_id,tool_call_id,tool_name,
			argument_fingerprint,before_version,after_timeline_id,after_version,
			terminal_status,result_json,created_at
		) VALUES(?,?,?,?,?,?,?,0,?,1,'succeeded','not-json',?)`,
		"receipt_corrupt", draftID, sourceMessage, turnID, toolCallID,
		"timeline.update", fingerprint, draftID+":v1", now,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := service.prepareTimelineMutationReceipt(
		ctx, "timeline.update", input,
	); err == nil || !strings.Contains(err.Error(), "读取 tool receipt") {
		t.Fatalf("corrupt receipt err=%v", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	missingCtx := rushestools.WithToolCallID(cancelled, "call-missing")
	if _, _, _, err := service.prepareTimelineMutationReceipt(
		missingCtx, "timeline.update", input,
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled lookup err=%v", err)
	}
}

func TestAgentTurnRunPersistencePropagatesDatabaseFailure(t *testing.T) {
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{database: database}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	item := QueueItem{
		DraftID: "draft-closed-turn-run", ItemID: "message-closed-turn-run",
		Kind: QueueUserMessage,
	}
	if err := service.startAgentTurnRun(t.Context(), "turn-closed", item); err == nil {
		t.Fatal("turn start unexpectedly ignored closed database")
	}
	if err := service.finishAgentTurnRun(t.Context(), "turn-closed", "failed"); err == nil {
		t.Fatal("turn finish unexpectedly ignored closed database")
	}
	if err := service.interruptStaleAgentTurnRuns(t.Context()); err == nil {
		t.Fatal("startup interruption unexpectedly ignored closed database")
	}
}
