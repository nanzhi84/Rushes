//go:build e2e_scaffold

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

const (
	e2eBlockUntilCancelMarker = "E2E_BLOCK_UNTIL_CANCEL"
	e2eFullMainlineMarker     = "E2E_FULL_MAINLINE"
	e2eMemoryWriteMarker      = "E2E_MEMORY_WRITE"
	e2eMemoryStatusMarker     = "E2E_MEMORY_STATUS"
	e2eShotSearchMarker       = "E2E_SHOT_SEARCH"
)

type e2eFallbackScaffold struct {
	service *Service
}

func newFallbackScaffold(service *Service) fallbackScaffold {
	return &e2eFallbackScaffold{service: service}
}

func (scaffold *e2eFallbackScaffold) TryHandle(
	ctx context.Context,
	draftID, _ string,
	content string,
) (string, bool, error) {
	switch {
	case strings.Contains(content, e2eBlockUntilCancelMarker):
		<-ctx.Done()
		return "", true, ctx.Err()
	case strings.Contains(content, e2eFullMainlineMarker):
		reply, err := scaffold.service.fallbackMainline(ctx, draftID)
		return reply, true, err
	case strings.Contains(content, e2eMemoryWriteMarker):
		result, err := scaffold.service.ExecuteTool(ctx, "memory.set", rushestools.MemorySetInput{
			Entries: []rushestools.MemoryEntryInput{{
				Key: "e2e_pacing", Kind: "preference", Statement: "E2E 成片节奏偏快",
				// evidence_quote 必是证据用户消息的原文子串；本回合证据就是这条 E2E_MEMORY_WRITE 消息。
				EvidenceQuote: e2eMemoryWriteMarker,
			}},
		})
		if err != nil {
			return "", true, err
		}
		toolResult, ok := result.(rushestools.ToolResult)
		if !ok || toolResult.Status != "succeeded" {
			return "", true, errors.New("E2E 长期记忆写入失败")
		}
		return "E2E_MEMORY_STORED", true, nil
	case strings.Contains(content, e2eMemoryStatusMarker):
		build, err := scaffold.service.contextManager.Build(ctx, draftID)
		if err != nil {
			return "", true, err
		}
		section, _ := build.Snapshot.Sections["user_memory"].(map[string]any)
		for _, entry := range agentexec.WorldStateObjectSlice(section["entries"]) {
			if entry["key"] == "e2e_pacing" {
				return "E2E_MEMORY_PRESENT", true, nil
			}
		}
		return "E2E_MEMORY_ABSENT", true, nil
	case strings.Contains(content, e2eShotSearchMarker):
		result, err := scaffold.service.ExecuteTool(ctx, "shot.search", rushestools.ShotSearchInput{
			Query: "测试画面", TopK: 5,
		})
		if err != nil {
			return "", true, err
		}
		search, ok := result.(rushestools.ShotSearchResult)
		if !ok || search.Status != string(rushestools.StatusSucceeded) || !search.SearchReady {
			return "", true, fmt.Errorf("E2E shot.search 未在完整快照成功: %#v", result)
		}
		return fmt.Sprintf(
			"E2E_SHOT_SEARCH_OK snapshot=%s total=%d returned=%d frozen=%d",
			search.IndexSnapshotID, search.TotalMatches, len(search.Shots), len(search.FrozenAssetIDs),
		), true, nil
	default:
		return "", false, nil
	}
}
