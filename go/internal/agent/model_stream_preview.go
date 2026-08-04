package agent

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
)

// modelStreamPreviewSession 把 provider 的实时正文与持久化终态拆开：正文分片可以立即
// 出现在 UI；完整消息到达后，含 tool_call 的消息固化为 narration，纯文本消息则等待
// terminal guard 通过后由 runTurn 原子晋升为最终 reply。失败或重试会显式撤销预览。
type modelStreamPreviewSession struct {
	mu sync.Mutex

	service        *Service
	draftID        string
	finalMessageID string
	finalCandidate string
	finalized      bool
}

type modelStreamPreviewContextKey struct{}

func newModelStreamPreviewSession(
	service *Service,
	draftID, finalMessageID string,
) *modelStreamPreviewSession {
	return &modelStreamPreviewSession{
		service: service, draftID: draftID, finalMessageID: finalMessageID,
	}
}

func withModelStreamPreviewSession(
	ctx context.Context,
	session *modelStreamPreviewSession,
) context.Context {
	if session == nil {
		return ctx
	}
	return context.WithValue(ctx, modelStreamPreviewContextKey{}, session)
}

func modelStreamPreviewFromContext(ctx context.Context) *modelStreamPreviewSession {
	session, _ := ctx.Value(modelStreamPreviewContextKey{}).(*modelStreamPreviewSession)
	return session
}

func (session *modelStreamPreviewSession) begin() string {
	if session == nil {
		return ""
	}
	return agentexec.RandomID("msg_preview")
}

func (session *modelStreamPreviewSession) delta(messageID, delta string) {
	if session == nil || messageID == "" || delta == "" {
		return
	}
	session.service.hub.Record(session.draftID, StreamEvent{
		"type": TurnStreamTextDelta, "message_id": messageID,
		"kind": "assistant", "delta": delta,
	})
}

func (session *modelStreamPreviewSession) discard(messageID string) {
	if session == nil || messageID == "" {
		return
	}
	session.service.hub.Record(session.draftID, StreamEvent{
		"type": TurnStreamMessageDiscarded, "message_id": messageID,
	})
}

func (session *modelStreamPreviewSession) complete(
	ctx context.Context,
	messageID string,
	response *schema.Message,
) {
	if session == nil || messageID == "" {
		return
	}
	if ctx.Err() != nil {
		session.discard(messageID)
		return
	}
	content := ""
	hasToolCalls := false
	if response != nil {
		content = response.Content
		hasToolCalls = len(response.ToolCalls) > 0
	}
	if strings.TrimSpace(content) == "" {
		session.discard(messageID)
	} else if hasToolCalls {
		if err := session.persistNarration(ctx, messageID, content); err != nil {
			slog.Error("持久化模型流式叙述失败",
				"draft_id", session.draftID, "message_id", messageID, "error", err)
		}
		session.service.hub.Record(session.draftID, StreamEvent{
			"type": TurnStreamMessageCompleted, "message_id": messageID,
			"kind": "narration", "content": content,
		})
	} else {
		staleCandidate := ""
		session.mu.Lock()
		if session.finalCandidate != messageID {
			staleCandidate = session.finalCandidate
		}
		session.finalCandidate = messageID
		session.mu.Unlock()
		// 自动终验或其他图分支可能让模型在同一回合产出多个纯文本终态候选。
		// 只有最新候选能被最终 message_completed 原子晋升；旧候选必须立即撤销，
		// 否则它会永久以 data-streaming=true 的纯文本残留在当前页面。
		session.discard(staleCandidate)
	}
	slog.Info("model_stream_message_completed",
		"draft_id", session.draftID,
		"preview_message_id", messageID,
		"final_message_id", session.finalMessageID,
		"visible_runes", utf8.RuneCountInString(content),
		"has_tool_calls", hasToolCalls,
	)
}

func (session *modelStreamPreviewSession) persistNarration(
	ctx context.Context,
	messageID, content string,
) error {
	result, err := reducer.Apply(ctx, session.service.database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: messageID, DraftID: session.draftID,
			Role: "assistant", Kind: "narration", Content: content,
		}},
	})
	if err != nil {
		return err
	}
	if result.Status != reducer.StatusApplied {
		return &previewPersistenceError{status: string(result.Status)}
	}
	return nil
}

type previewPersistenceError struct{ status string }

func (err *previewPersistenceError) Error() string {
	return "model preview reducer status: " + err.status
}

func (session *modelStreamPreviewSession) candidate() string {
	if session == nil {
		return ""
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.finalCandidate
}

func (session *modelStreamPreviewSession) finalize() {
	if session == nil {
		return
	}
	session.mu.Lock()
	session.finalized = true
	session.mu.Unlock()
}

// discardPending 兜住模型已经给出纯文本、但后续 terminal guard、持久化或取消失败的路径。
// 成功路径会先用 message_completed 的 replaces_message_id 原子替换，再标记 finalized。
func (session *modelStreamPreviewSession) discardPending() {
	if session == nil {
		return
	}
	session.mu.Lock()
	if session.finalized || session.finalCandidate == "" {
		session.mu.Unlock()
		return
	}
	messageID := session.finalCandidate
	session.finalCandidate = ""
	session.mu.Unlock()
	session.discard(messageID)
}
