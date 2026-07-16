package storage

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	UserMemoryLimit              = 50
	UserMemoryStatementRuneLimit = 200
	UserMemoryEvidenceMessage    = "user_message"
	UserMemoryEvidenceDecision   = "decision_answer"
)

var userMemoryKeyPattern = regexp.MustCompile(`^[a-z0-9_]{2,40}$`)

type UserMemory struct {
	Key             string `json:"key"`
	Kind            string `json:"kind"`
	Statement       string `json:"statement"`
	EvidenceKind    string `json:"evidence_kind"`
	EvidenceID      string `json:"evidence_id"`
	SourceDraftID   string `json:"source_draft_id"`
	CreatedAt       string `json:"created_at"`
	LastConfirmedAt string `json:"last_confirmed_at"`
}

func ValidUserMemoryKey(value string) bool {
	return userMemoryKeyPattern.MatchString(value)
}

func ValidUserMemoryKind(value string) bool {
	switch value {
	case "preference", "correction", "habit":
		return true
	default:
		return false
	}
}

func ValidUserMemoryStatement(value string) bool {
	return strings.TrimSpace(value) != "" && utf8.RuneCountInString(value) <= UserMemoryStatementRuneLimit
}

func ValidUserMemoryEvidenceKind(value string) bool {
	return value == UserMemoryEvidenceMessage || value == UserMemoryEvidenceDecision
}

func ListUserMemories(ctx context.Context, query Querier) ([]UserMemory, error) {
	rows, err := query.QueryContext(ctx, `
		SELECT memory_key,kind,statement,evidence_kind,evidence_id,
		source_draft_id,created_at,last_confirmed_at
		FROM user_memories
		ORDER BY last_confirmed_at DESC,memory_key ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	memories := []UserMemory{}
	for rows.Next() {
		var memory UserMemory
		if err := rows.Scan(
			&memory.Key, &memory.Kind, &memory.Statement,
			&memory.EvidenceKind, &memory.EvidenceID, &memory.SourceDraftID,
			&memory.CreatedAt, &memory.LastConfirmedAt,
		); err != nil {
			return nil, err
		}
		memories = append(memories, memory)
	}
	return memories, rows.Err()
}
