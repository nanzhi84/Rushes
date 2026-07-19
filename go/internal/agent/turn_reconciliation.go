package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

type persistedTurnCandidate struct {
	draftID        string
	itemID         string
	kind           QueueItemKind
	content        string
	decision       storage.Decision
	jobObservation bridgeObservation
	created        time.Time
	recoverable    bool
}

type persistedTurnFact struct {
	created           time.Time
	candidate         *persistedTurnCandidate
	decisionCreatedID string
	terminal          bool
}

// ReconcilePersistedTurns 补驱“reducer 已提交、内存 TurnQueue 未接住”的 user 消息和
// decision answer。持久 messages/decisions 是唯一事实源，不另建 turn-intent 表；队列的
// idle 守卫让重复调用幂等。调用方应在单实例启动、job bridge 与 HTTP 监听启动前执行。
func (service *Service) ReconcilePersistedTurns(ctx context.Context) error {
	if service == nil || service.database == nil || service.queue == nil {
		return errors.New("启动对账缺少 agent service 依赖")
	}
	candidates, err := service.persistedTurnCandidates(ctx)
	if err != nil {
		return err
	}
	recoveredDrafts := map[string]bool{}
	for _, candidate := range candidates {
		if recovered, seen := recoveredDrafts[candidate.draftID]; seen {
			if recovered {
				service.enqueuePersistedTurn(ctx, candidate, false)
			}
			continue
		}
		recoveredDrafts[candidate.draftID] = service.enqueuePersistedTurn(ctx, candidate, true)
	}
	return nil
}

func (service *Service) enqueuePersistedTurn(
	ctx context.Context,
	candidate persistedTurnCandidate,
	onlyIfIdle bool,
) bool {
	if candidate.kind == QueueUserMessage {
		if onlyIfIdle {
			return service.queue.EnqueueUserMessageIfIdle(
				candidate.draftID, candidate.itemID, candidate.content,
			)
		}
		return service.queue.EnqueueUserMessage(candidate.draftID, candidate.itemID, candidate.content)
	}
	if candidate.kind == QueueJobObservation {
		return service.enqueueJobObservation(ctx, candidate.jobObservation, onlyIfIdle)
	}
	if candidate.kind != QueueUIObservation {
		return false
	}
	itemID := candidate.decision.ID
	if candidate.decision.ReplayedToolCallID != nil && *candidate.decision.ReplayedToolCallID != "" {
		itemID = *candidate.decision.ReplayedToolCallID
	}
	payload := map[string]any{
		"decision_id":       candidate.decision.ID,
		"answer":            candidate.decision.Answer,
		"pending_tool_call": candidate.decision.PendingToolCall,
	}
	if onlyIfIdle {
		return service.queue.EnqueueUIObservationIfIdle(
			candidate.draftID, itemID, "decision_answered", payload,
		)
	}
	return service.queue.EnqueueUIObservation(
		candidate.draftID, itemID, "decision_answered", payload,
	)
}

func (service *Service) persistedTurnCandidates(ctx context.Context) ([]persistedTurnCandidate, error) {
	facts := map[string][]persistedTurnFact{}
	userRows, err := service.database.Read().QueryContext(ctx, `
		SELECT m.draft_id,m.message_id,m.content,m.created_at
		FROM messages m JOIN drafts d ON d.draft_id=m.draft_id
		WHERE d.status!='trashed' AND m.rewound_at IS NULL
		AND m.rowid >= COALESCE((
			SELECT anchor.rowid FROM messages anchor WHERE anchor.message_id=d.messages_tail_ref
		),0)
		AND m.role='user'
		ORDER BY m.created_at,m.rowid`)
	if err != nil {
		return nil, err
	}
	for userRows.Next() {
		var candidate persistedTurnCandidate
		var created string
		candidate.kind = QueueUserMessage
		candidate.recoverable = true
		if err := userRows.Scan(
			&candidate.draftID, &candidate.itemID, &candidate.content, &created,
		); err != nil {
			_ = userRows.Close()
			return nil, err
		}
		candidate.created, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			_ = userRows.Close()
			return nil, fmt.Errorf("解析遗留 user 消息时间 %s: %w", candidate.itemID, err)
		}
		copy := candidate
		facts[candidate.draftID] = append(facts[candidate.draftID], persistedTurnFact{
			created: candidate.created, candidate: &copy,
		})
	}
	if err := userRows.Err(); err != nil {
		_ = userRows.Close()
		return nil, err
	}
	if err := userRows.Close(); err != nil {
		return nil, err
	}

	eventRows, err := service.database.Read().QueryContext(ctx, `
		SELECT e.draft_id,e.payload_json,e.created_at
		FROM event_log e
		JOIN drafts draft ON draft.draft_id=e.draft_id
		LEFT JOIN messages anchor ON anchor.message_id=draft.messages_tail_ref
		WHERE e.event_type IN ('DecisionCreated','DecisionAnswered') AND draft.status!='trashed'
		AND (draft.messages_tail_ref IS NULL OR e.created_at>anchor.created_at)
		AND NOT EXISTS(
			SELECT 1 FROM event_log rewind
			WHERE rewind.draft_id=e.draft_id
			AND rewind.event_type='TimelineVersionRestored'
			AND rewind.event_id>e.event_id
		)
		ORDER BY e.created_at,e.event_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = eventRows.Close() }()
	for eventRows.Next() {
		var draftID, payloadJSON, created string
		if err := eventRows.Scan(&draftID, &payloadJSON, &created); err != nil {
			return nil, err
		}
		createdAt, err := time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, fmt.Errorf("解析遗留 decision 事件时间: %w", err)
		}
		var event contracts.Event
		if err := json.Unmarshal([]byte(payloadJSON), &event); err != nil {
			return nil, fmt.Errorf("解析 decision 事件: %w", err)
		}
		decisionID, _ := event.Payload["decision_id"].(string)
		if decisionID == "" {
			continue
		}
		if event.Type == "DecisionCreated" {
			blocking, _ := event.Payload["blocking"].(bool)
			if blocking {
				facts[draftID] = append(facts[draftID], persistedTurnFact{
					created: createdAt, decisionCreatedID: decisionID,
				})
			}
			continue
		}
		decision, err := storage.GetDecision(ctx, service.database.Read(), decisionID)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if decision.Status != "answered" || decision.DraftID == nil || *decision.DraftID != draftID {
			continue
		}
		candidate := persistedTurnCandidate{
			draftID: draftID, itemID: decisionID, kind: QueueUIObservation,
			decision: decision, created: createdAt, recoverable: true,
		}
		copy := candidate
		facts[draftID] = append(facts[draftID], persistedTurnFact{
			created: candidate.created, candidate: &copy,
		})
	}
	if err := eventRows.Err(); err != nil {
		return nil, err
	}

	deliveredJobTerminals := map[string]int{}
	deliveryRows, err := service.database.Read().QueryContext(ctx, `
		SELECT event_id,job_id,draft_id,event_json,claim_token,created_at,delivered_at
		FROM agent_job_observations`)
	if err != nil {
		return nil, err
	}
	for deliveryRows.Next() {
		var eventID int64
		var jobID, draftID, eventJSON, claimToken, created string
		var deliveredAt sql.NullString
		if err := deliveryRows.Scan(
			&eventID, &jobID, &draftID, &eventJSON, &claimToken, &created, &deliveredAt,
		); err != nil {
			_ = deliveryRows.Close()
			return nil, err
		}
		createdAt, err := time.Parse(time.RFC3339Nano, created)
		if err != nil {
			_ = deliveryRows.Close()
			return nil, fmt.Errorf("解析 job observation 创建时间: %w", err)
		}
		jobCandidate := persistedTurnCandidate{
			draftID: draftID, itemID: jobID, kind: QueueJobObservation,
			created: createdAt, recoverable: !deliveredAt.Valid,
		}
		jobCandidate.jobObservation = bridgeObservation{
			eventID: eventID, draftID: draftID, jobID: jobID, claimToken: claimToken,
		}
		_ = json.Unmarshal([]byte(eventJSON), &jobCandidate.jobObservation.event)
		facts[draftID] = append(facts[draftID], persistedTurnFact{
			created: createdAt, candidate: &jobCandidate,
		})
		if !deliveredAt.Valid {
			continue
		}
		deliveredJobTerminals[draftID+"\x00"+deliveredAt.String]++
		deliveredTime, err := time.Parse(time.RFC3339Nano, deliveredAt.String)
		if err != nil {
			_ = deliveryRows.Close()
			return nil, fmt.Errorf("解析 job observation 交付时间: %w", err)
		}
		facts[draftID] = append(facts[draftID], persistedTurnFact{
			created: deliveredTime, terminal: true,
		})
	}
	if err := deliveryRows.Err(); err != nil {
		_ = deliveryRows.Close()
		return nil, err
	}
	if err := deliveryRows.Close(); err != nil {
		return nil, err
	}

	terminalRows, err := service.database.Read().QueryContext(ctx, `
		SELECT m.draft_id,m.created_at
		FROM messages m JOIN drafts d ON d.draft_id=m.draft_id
		WHERE d.status!='trashed' AND m.rewound_at IS NULL
		AND m.rowid >= COALESCE((
			SELECT anchor.rowid FROM messages anchor WHERE anchor.message_id=d.messages_tail_ref
		),0)
		AND (
			(m.role='assistant' AND m.kind='reply')
			OR (m.role='system' AND m.kind='turn_failure')
			OR (m.role='system_observation' AND m.kind='turn_cancelled')
		)
		ORDER BY m.created_at,m.rowid`)
	if err != nil {
		return nil, err
	}
	for terminalRows.Next() {
		var draftID, created string
		if err := terminalRows.Scan(&draftID, &created); err != nil {
			_ = terminalRows.Close()
			return nil, err
		}
		jobKey := draftID + "\x00" + created
		if deliveredJobTerminals[jobKey] > 0 {
			deliveredJobTerminals[jobKey]--
			continue
		}
		createdAt, err := time.Parse(time.RFC3339Nano, created)
		if err != nil {
			_ = terminalRows.Close()
			return nil, fmt.Errorf("解析回合终态消息时间: %w", err)
		}
		facts[draftID] = append(facts[draftID], persistedTurnFact{
			created: createdAt, terminal: true,
		})
	}
	if err := terminalRows.Err(); err != nil {
		_ = terminalRows.Close()
		return nil, err
	}
	if err := terminalRows.Close(); err != nil {
		return nil, err
	}

	pendingDrafts := map[string]bool{}
	pendingRows, err := service.database.Read().QueryContext(ctx, `
		SELECT draft_id FROM drafts WHERE status!='trashed' AND pending_decision_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	for pendingRows.Next() {
		var draftID string
		if err := pendingRows.Scan(&draftID); err != nil {
			_ = pendingRows.Close()
			return nil, err
		}
		pendingDrafts[draftID] = true
	}
	if err := pendingRows.Err(); err != nil {
		_ = pendingRows.Close()
		return nil, err
	}
	if err := pendingRows.Close(); err != nil {
		return nil, err
	}

	candidates := make([]persistedTurnCandidate, 0, len(facts))
	draftIDs := make([]string, 0, len(facts))
	for draftID := range facts {
		draftIDs = append(draftIDs, draftID)
	}
	sort.Strings(draftIDs)
	for _, draftID := range draftIDs {
		if pendingDrafts[draftID] {
			continue
		}
		timeline := facts[draftID]
		sort.SliceStable(timeline, func(i, j int) bool {
			if timeline[i].created.Equal(timeline[j].created) {
				return timeline[i].candidate != nil && timeline[j].candidate == nil
			}
			return timeline[i].created.Before(timeline[j].created)
		})
		unmatched := make([]persistedTurnCandidate, 0)
		coalesceDecisionTerminal := ""
		blockingDecisions := map[string]bool{}
		for _, fact := range timeline {
			if fact.candidate != nil {
				if fact.candidate.kind == QueueUIObservation && blockingDecisions[fact.candidate.decision.ID] {
					// pending decision 期间内存队列中的 U2 会在崩溃时丢失，而用户回答会
					// 直接入队 continuation；若 U2 至今仍无终态，恢复顺序必须保持实际的
					// continuation→U2，避免下一 terminal 在二次崩溃时反向误配。
					if fact.candidate.decision.ID == coalesceDecisionTerminal {
						coalesceDecisionTerminal = ""
					}
					unmatched = append(
						[]persistedTurnCandidate{*fact.candidate}, unmatched...,
					)
					continue
				}
				unmatched = append(unmatched, *fact.candidate)
				continue
			}
			if fact.decisionCreatedID != "" {
				if len(unmatched) > 0 {
					unmatched = unmatched[1:]
				}
				// 正常路径会在 blocking DecisionCreated 后落一条等待提示；它与
				// DecisionCreated 是同一回合的两个持久事实，只能核销一次。
				coalesceDecisionTerminal = fact.decisionCreatedID
				blockingDecisions[fact.decisionCreatedID] = true
				continue
			}
			if fact.terminal && coalesceDecisionTerminal != "" {
				coalesceDecisionTerminal = ""
				continue
			}
			if len(unmatched) > 0 {
				unmatched = unmatched[1:]
			}
		}
		for _, candidate := range unmatched {
			if candidate.recoverable {
				candidates = append(candidates, candidate)
			}
		}
	}
	return candidates, nil
}
