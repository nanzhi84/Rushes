package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type terminalTimelineTruthContextKey struct{}

type terminalTimelineTruthState struct {
	mu sync.Mutex

	mutationSequence     uint64
	mutationTimelineID   string
	mutationProofInvalid bool
	checkSequence        uint64
	checkTimelineID      string
	checkProofInvalid    bool
}

type terminalTimelineTruthSnapshot struct {
	mutationSequence     uint64
	mutationTimelineID   string
	mutationProofInvalid bool
	checkSequence        uint64
	checkTimelineID      string
	checkProofInvalid    bool
}

type terminalReplyGuardError struct {
	kind               string
	mutationTimelineID string
	checkTimelineID    string
	latestTimelineID   string
	details            string
}

func (guardErr *terminalReplyGuardError) Error() string {
	switch guardErr.kind {
	case "tool_policy_unresolved":
		return "工具策略确认尚未解决：" + guardErr.details
	case "timeline_check_missing":
		return fmt.Sprintf("最新编辑 %s 尚未通过同版本 timeline.check", guardErr.mutationTimelineID)
	case "timeline_check_stale":
		return fmt.Sprintf(
			"最新编辑 %s 的终态检查已过期（最后检查 %s）",
			guardErr.mutationTimelineID,
			guardErr.checkTimelineID,
		)
	case "timeline_mutation_unverified":
		return "时间线编辑成功结果缺少有效的 timeline_id，无法绑定实际写入版本"
	case "timeline_check_unverified":
		return "时间线检查成功结果缺少有效的 timeline_id，无法绑定实际检查版本"
	case "timeline_latest_changed":
		return fmt.Sprintf(
			"终态检查后时间线已变化（编辑 %s，当前最新 %s）",
			guardErr.mutationTimelineID,
			guardErr.latestTimelineID,
		)
	case "terminal_late_tool_call":
		return "终态回复中包含未执行的工具调用"
	default:
		return "终态真值门禁未通过"
	}
}

func newTerminalTimelineTruthState() *terminalTimelineTruthState {
	return &terminalTimelineTruthState{}
}

func withTerminalTimelineTruthState(
	ctx context.Context,
	state *terminalTimelineTruthState,
) context.Context {
	return context.WithValue(ctx, terminalTimelineTruthContextKey{}, state)
}

func terminalTimelineTruthFromContext(ctx context.Context) *terminalTimelineTruthState {
	state, _ := ctx.Value(terminalTimelineTruthContextKey{}).(*terminalTimelineTruthState)
	return state
}

func (state *terminalTimelineTruthState) recordToolResult(name, status string, output any) {
	if state == nil || status != "succeeded" {
		return
	}
	result, ok := terminalTruthToolResult(output)
	if !ok || result.Status != string(rushestools.StatusSucceeded) {
		return
	}
	timelineID := agentexec.InterfaceString(result.Data["timeline_id"])
	state.mu.Lock()
	defer state.mu.Unlock()
	switch {
	case isTerminalTimelineMutation(name):
		if !isValidTimelineVersionID(timelineID) {
			state.mutationProofInvalid = true
			return
		}
		state.mutationSequence++
		state.mutationTimelineID = timelineID
		state.mutationProofInvalid = false
	case name == "timeline.check":
		if !isValidTimelineVersionID(timelineID) {
			state.checkProofInvalid = true
			return
		}
		state.checkSequence = state.mutationSequence
		state.checkTimelineID = timelineID
		state.checkProofInvalid = false
	}
}

func isValidTimelineVersionID(timelineID string) bool {
	_, _, ok := splitTimelineID(timelineID)
	return ok
}

func isTerminalTimelineMutation(name string) bool {
	switch name {
	case "timeline.insert", "timeline.delete", "timeline.update", "timeline.split":
		return true
	default:
		return false
	}
}

func (state *terminalTimelineTruthState) snapshot() terminalTimelineTruthSnapshot {
	if state == nil {
		return terminalTimelineTruthSnapshot{}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return terminalTimelineTruthSnapshot{
		mutationSequence:     state.mutationSequence,
		mutationTimelineID:   state.mutationTimelineID,
		mutationProofInvalid: state.mutationProofInvalid,
		checkSequence:        state.checkSequence,
		checkTimelineID:      state.checkTimelineID,
		checkProofInvalid:    state.checkProofInvalid,
	}
}

func terminalTruthToolResult(output any) (rushestools.ToolResult, bool) {
	switch typed := output.(type) {
	case rushestools.ToolResult:
		return typed, true
	case *rushestools.ToolResult:
		if typed != nil {
			return *typed, true
		}
	}
	return rushestools.ToolResult{}, false
}

func (service *Service) terminalReplyGuard(ctx context.Context, draftID string) error {
	if recovery := toolRecoveryFromContext(ctx); recovery != nil && recovery.unresolved() {
		return &terminalReplyGuardError{
			kind: "tool_policy_unresolved", details: recovery.summary(),
		}
	}
	snapshot := terminalTimelineTruthFromContext(ctx).snapshot()
	if snapshot.mutationProofInvalid ||
		(snapshot.mutationSequence > 0 && snapshot.mutationTimelineID == "") {
		return &terminalReplyGuardError{kind: "timeline_mutation_unverified"}
	}
	if snapshot.checkProofInvalid {
		return &terminalReplyGuardError{kind: "timeline_check_unverified"}
	}
	expectedTimelineID := terminalExpectedTimelineID(snapshot)
	if expectedTimelineID == "" {
		return nil
	}
	if snapshot.mutationSequence > 0 &&
		(snapshot.checkSequence == 0 || snapshot.checkTimelineID == "") {
		return &terminalReplyGuardError{
			kind: "timeline_check_missing", mutationTimelineID: snapshot.mutationTimelineID,
		}
	}
	if snapshot.mutationSequence > 0 &&
		(snapshot.checkSequence != snapshot.mutationSequence ||
			snapshot.checkTimelineID != snapshot.mutationTimelineID) {
		return &terminalReplyGuardError{
			kind: "timeline_check_stale", mutationTimelineID: snapshot.mutationTimelineID,
			checkTimelineID: snapshot.checkTimelineID,
		}
	}
	latest, err := timeline.Latest(ctx, service.database, draftID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return &terminalReplyGuardError{
				kind: "timeline_latest_changed", mutationTimelineID: expectedTimelineID,
				latestTimelineID: "不存在",
			}
		}
		return err
	}
	if latest.TimelineID != expectedTimelineID {
		return &terminalReplyGuardError{
			kind: "timeline_latest_changed", mutationTimelineID: expectedTimelineID,
			latestTimelineID: latest.TimelineID,
		}
	}
	return nil
}

// terminalExpectedTimelineID 统一终态门禁与最终消息事务的版本绑定：本轮有编辑时
// 以最后编辑版本为准；纯检查回合也必须绑定最后成功检查的版本，不能把 v1 的检查
// 结论提交到已经前进到 v2 的草稿上。没有编辑也没有检查的普通对话无需绑定。
func terminalExpectedTimelineID(snapshot terminalTimelineTruthSnapshot) string {
	if snapshot.mutationSequence > 0 {
		return snapshot.mutationTimelineID
	}
	return snapshot.checkTimelineID
}

// terminalTimelineLatestValidation 在最终消息的 reducer immediate transaction 内重新读取
// 当前版本。断言成功后到消息 commit 之间持有同一写事务，因此其他编辑不可能插入竞态窗口。
func terminalTimelineLatestValidation(
	draftID, expectedTimelineID string,
) func(context.Context, *sql.Tx, []string) error {
	return func(ctx context.Context, tx *sql.Tx, _ []string) error {
		var latestTimelineID sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT t.timeline_id
			FROM drafts AS d
			LEFT JOIN timeline_versions AS t
				ON t.draft_id=d.draft_id AND t.version=d.timeline_current_version
			WHERE d.draft_id=?`, draftID,
		).Scan(&latestTimelineID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !latestTimelineID.Valid) {
			return &terminalReplyGuardError{
				kind: "timeline_latest_changed", mutationTimelineID: expectedTimelineID,
				latestTimelineID: "不存在",
			}
		}
		if err != nil {
			return err
		}
		if latestTimelineID.String != expectedTimelineID {
			return &terminalReplyGuardError{
				kind: "timeline_latest_changed", mutationTimelineID: expectedTimelineID,
				latestTimelineID: latestTimelineID.String,
			}
		}
		return nil
	}
}

func (service *Service) timelineLatestChangedError(
	ctx context.Context, draftID, expectedTimelineID string,
) error {
	latestTimelineID := "不存在"
	latest, err := timeline.Latest(ctx, service.database, draftID)
	if err == nil {
		latestTimelineID = latest.TimelineID
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	return &terminalReplyGuardError{
		kind: "timeline_latest_changed", mutationTimelineID: expectedTimelineID,
		latestTimelineID: latestTimelineID,
	}
}
