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
	// UserMemoryEvidenceQuoteMinRunes 是 evidence_quote 的最短长度下限。子串校验
	// 是防污染主闸门；这里再挡掉单字符这类退化匹配（任何含该字的消息都会命中），
	// 又不至于误伤「太快」「快点」这类合法的短纠正原文。
	UserMemoryEvidenceQuoteMinRunes = 2
	UserMemoryEvidenceMessage       = "user_message"
	UserMemoryEvidenceDecision      = "decision_answer"
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
	// LastUsedAt 是该记忆最近一次「被注入 WorldState 且回合成功收尾」的时间；
	// 历史行为空串（列可空）。淘汰价值按 max(last_confirmed_at,last_used_at) 衡量，
	// 让长期只读的稳定偏好不因久未复写而最先出局。
	LastUsedAt string `json:"last_used_at,omitempty"`
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

// ValidUserMemoryEvidenceQuote 只做形状校验（去空白后长度下限）；quote 是否确为
// 证据原文子串由 reducer 事务内比对，避免在无数据库上下文的调用点做半套校验。
func ValidUserMemoryEvidenceQuote(value string) bool {
	return utf8.RuneCountInString(strings.TrimSpace(value)) >= UserMemoryEvidenceQuoteMinRunes
}

func ValidUserMemoryEvidenceKind(value string) bool {
	return value == UserMemoryEvidenceMessage || value == UserMemoryEvidenceDecision
}

func ListUserMemories(ctx context.Context, query Querier) ([]UserMemory, error) {
	rows, err := query.QueryContext(ctx, `
		SELECT memory_key,kind,statement,evidence_kind,evidence_id,
		source_draft_id,created_at,last_confirmed_at,COALESCE(last_used_at,'')
		FROM user_memories
		ORDER BY max(last_confirmed_at,COALESCE(last_used_at,last_confirmed_at)) DESC,memory_key ASC`)
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
			&memory.CreatedAt, &memory.LastConfirmedAt, &memory.LastUsedAt,
		); err != nil {
			return nil, err
		}
		memories = append(memories, memory)
	}
	return memories, rows.Err()
}
