package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type auxiliaryTimelineViewProviderCall struct {
	messages      []*schema.Message
	toolForbidden bool
	draftID       string
	leaseSession  *timelineEditLeaseSession
}

type auxiliaryTimelineViewProviderSpy struct {
	calls []auxiliaryTimelineViewProviderCall
}

func (spy *auxiliaryTimelineViewProviderSpy) WithTools(
	[]*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	return spy, nil
}

func (spy *auxiliaryTimelineViewProviderSpy) Generate(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.Message, error) {
	common := model.GetCommonOptions(&model.Options{}, options...)
	draftID, _ := rushestools.DraftID(ctx)
	spy.calls = append(spy.calls, auxiliaryTimelineViewProviderCall{
		messages:      append([]*schema.Message(nil), messages...),
		toolForbidden: common.ToolChoice != nil && *common.ToolChoice == schema.ToolChoiceForbidden,
		draftID:       draftID,
		leaseSession:  timelineEditLeaseSessionFromContext(ctx),
	})
	for _, message := range messages {
		if message.Role == schema.System && strings.Contains(message.Content, "回复整形器") {
			return schema.AssistantMessage("已按当前时间线完成编辑。", nil), nil
		}
	}
	return schema.AssistantMessage("COMPACTED-WITH-CURRENT-VIEW", nil), nil
}

func (spy *auxiliaryTimelineViewProviderSpy) Stream(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	panic("auxiliary provider calls must use Generate")
}

func TestContextCompactionProviderGetsExactlyOneCurrentTimelineView(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft-aux-view-compaction"
	agenttest.CreateAgentDraft(t, database, draftID)
	spy := &auxiliaryTimelineViewProviderSpy{}
	service, err := NewService(t.Context(), database, spy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedAuxiliaryTimelineVersion(t, service, t.Context(), draftID, 1, "manual")

	insertContextMessage(t, database, storage.Message{
		ID: "assistant_compaction_source", DraftID: draftID,
		Role: "assistant", Kind: "reply",
		Content: strings.Repeat("需要压缩的旧上下文", contextHistorySoftTokenLimit),
	})
	insertContextMessage(t, database, storage.Message{
		ID: "user_compaction_pending", DraftID: draftID,
		Role: "user", Kind: "user", Content: "保留最新请求",
	})
	ctx := withTestTurnLeaseSession(t, service, t.Context(), draftID)
	session := timelineEditLeaseSessionFromContext(ctx)
	if err := session.ensure(ctx); err != nil {
		t.Fatal(err)
	}

	messages, err := service.modelMessages(ctx, draftID)
	if err != nil {
		t.Fatal(err)
	}
	if len(spy.calls) != 1 {
		t.Fatalf("compaction provider calls=%d want=1", len(spy.calls))
	}
	assertAuxiliaryTimelineViewCall(t, spy.calls[0], draftID, session, 1)
	if got := countTimelineViewMessages(messages); got != 0 {
		t.Fatalf("CurrentTimelineView leaked into persisted ReAct/context transcript: %d", got)
	}
	if !strings.Contains(joinMessageContent(messages), "COMPACTED-WITH-CURRENT-VIEW") ||
		messages[len(messages)-1].Content != "保留最新请求" {
		t.Fatalf("compaction result/tail changed: %#v", messages)
	}
	stored, err := storage.ListMessages(t.Context(), database.Read(), draftID, 20)
	if err != nil || len(stored) != 2 {
		t.Fatalf("compaction must not rewrite message transcript: len=%d err=%v", len(stored), err)
	}
}

func TestReflectionRestatementProviderGetsExactlyOneFreshCurrentTimelineView(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft-aux-view-reflection"
	agenttest.CreateAgentDraft(t, database, draftID)
	spy := &auxiliaryTimelineViewProviderSpy{}
	service, err := NewService(t.Context(), database, spy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedAuxiliaryTimelineVersion(t, service, t.Context(), draftID, 1, "manual")

	ctx := withTestTurnLeaseSession(t, service, t.Context(), draftID)
	session := timelineEditLeaseSessionFromContext(ctx)
	seedAuxiliaryTimelineVersion(t, service, ctx, draftID, 2, "agent")
	before, err := storage.ListMessages(t.Context(), database.Read(), draftID, 20)
	if err != nil {
		t.Fatal(err)
	}
	original := "时间线已经改完。但等等，我需要重新确认。"
	out, restated := service.qualityCheckedFinalReply(ctx, draftID, "message-reflection", original)
	if out != "已按当前时间线完成编辑。" || !restated {
		t.Fatalf("out=%q restated=%v", out, restated)
	}
	if len(spy.calls) != 1 {
		t.Fatalf("reflection provider calls=%d want=1", len(spy.calls))
	}
	assertAuxiliaryTimelineViewCall(t, spy.calls[0], draftID, session, 2)
	callMessages := spy.calls[0].messages
	if len(callMessages) != 3 || callMessages[0].Role != schema.System ||
		callMessages[1].Role != schema.System || callMessages[2].Role != schema.User ||
		callMessages[2].Content != original {
		t.Fatalf("reflection provider transcript changed: %#v", callMessages)
	}
	after, err := storage.ListMessages(t.Context(), database.Read(), draftID, 20)
	if err != nil || len(after) != len(before) {
		t.Fatalf("reflection helper must not mutate ReAct transcript: before=%d after=%d err=%v",
			len(before), len(after), err)
	}
}

func seedAuxiliaryTimelineVersion(
	t *testing.T,
	service *Service,
	ctx context.Context,
	draftID string,
	version int,
	origin string,
) {
	t.Helper()
	document, err := agenttest.ComposeTimeline(draftID, version, []agenttest.TimelineSelection{{
		AssetID: "asset-aux-view", AssetKind: "video",
		SourceStartFrame: 0, SourceEndFrame: 60, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if origin == "manual" {
		ctx = rushestools.WithTimelineMutationOrigin(ctx, "manual")
	}
	result, err := seedTimelineVersion(
		service, ctx, draftID, document, "auxiliary_current_timeline_view", nil,
	)
	if err != nil || result.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("seed v%d result=%#v err=%v", version, result, err)
	}
}

func assertAuxiliaryTimelineViewCall(
	t *testing.T,
	call auxiliaryTimelineViewProviderCall,
	draftID string,
	session *timelineEditLeaseSession,
	version int,
) {
	t.Helper()
	if !call.toolForbidden {
		t.Fatal("auxiliary provider call must forbid tools")
	}
	if call.draftID != draftID || call.leaseSession != session {
		t.Fatalf("provider did not reuse turn context: draft=%q session=%p want=%p",
			call.draftID, call.leaseSession, session)
	}
	var views []*schema.Message
	for _, message := range call.messages {
		if phase, _ := message.Extra["context_phase"].(string); phase == currentTimelineViewContextPhase {
			views = append(views, message)
		}
	}
	if len(views) != 1 {
		t.Fatalf("CurrentTimelineView count=%d want=1", len(views))
	}
	view := decodeCurrentTimelineViewMessage(t, views[0].Content)
	if view["draft_id"] != draftID || view["version"] != float64(version) ||
		view["timeline_id"] != fmt.Sprintf("%s:v%d", draftID, version) ||
		view["edit_lease_turn_id"] != session.turnID {
		t.Fatalf("CurrentTimelineView is not the turn's latest SQLite snapshot: %#v", view)
	}
}

func countTimelineViewMessages(messages []*schema.Message) int {
	count := 0
	for _, message := range messages {
		if phase, _ := message.Extra["context_phase"].(string); phase == currentTimelineViewContextPhase {
			count++
		}
	}
	return count
}
