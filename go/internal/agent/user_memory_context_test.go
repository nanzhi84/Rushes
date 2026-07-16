package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func TestUserMemoryWorldStateIsStableAcrossDraftsAndRemoval(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_memory_source")
	createAgentDraft(t, database, "draft_memory_target")
	insertAgentMessage(t, database, "draft_memory_source", "message_memory_source", "以后成片节奏都快一点")

	applyUserMemories(t, database, []reducer.UserMemoryRow{{
		Key: "pacing", Kind: "preference", Statement: "成片节奏偏快",
		EvidenceKind: storage.UserMemoryEvidenceMessage,
		EvidenceID:   "message_memory_source", SourceDraftID: "draft_memory_source",
	}}, nil)

	builder := NewContextBuilder(database)
	snapshot, err := builder.Snapshot(t.Context(), "draft_memory_target")
	if err != nil {
		t.Fatal(err)
	}
	section := userMemorySection(t, snapshot)
	entries := worldStateObjectSlice(section["entries"])
	if len(entries) != 1 || entries[0]["key"] != "pacing" ||
		entries[0]["kind"] != "preference" || entries[0]["statement"] != "成片节奏偏快" {
		t.Fatalf("user_memory entries=%#v", entries)
	}
	for _, privateField := range []string{"evidence_kind", "evidence_id", "source_draft_id", "created_at", "last_confirmed_at"} {
		if _, exists := entries[0][privateField]; exists {
			t.Fatalf("user_memory leaked private field %q: %#v", privateField, entries[0])
		}
	}
	if total, ok := numericValue(section["total"]); !ok || total != 1 || section["truncated"] != false {
		t.Fatalf("user_memory metadata=%#v", section)
	}

	applyUserMemories(t, database, nil, []string{"pacing"})
	afterRemoval, err := builder.Snapshot(t.Context(), "draft_memory_target")
	if err != nil {
		t.Fatal(err)
	}
	section = userMemorySection(t, afterRemoval)
	if len(worldStateObjectSlice(section["entries"])) != 0 {
		t.Fatalf("removed memory still visible: %#v", section)
	}
	if total, ok := numericValue(section["total"]); !ok || total != 0 || section["truncated"] != false {
		t.Fatalf("empty user_memory metadata=%#v", section)
	}
}

func TestUserMemoryWorldStateUsesWholeEntryBudget(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_memory_budget")
	insertAgentMessage(t, database, "draft_memory_budget", "message_memory_budget", "请记住这些长期偏好")

	memories := make([]reducer.UserMemoryRow, 0, storage.UserMemoryLimit)
	for index := 0; index < storage.UserMemoryLimit; index++ {
		memories = append(memories, reducer.UserMemoryRow{
			Key: fmt.Sprintf("memory_%02d", index), Kind: "preference",
			Statement:    strings.Repeat("偏", storage.UserMemoryStatementRuneLimit),
			EvidenceKind: storage.UserMemoryEvidenceMessage,
			EvidenceID:   "message_memory_budget", SourceDraftID: "draft_memory_budget",
		})
	}
	applyUserMemories(t, database, memories, nil)

	snapshot, err := NewContextBuilder(database).Snapshot(t.Context(), "draft_memory_budget")
	if err != nil {
		t.Fatal(err)
	}
	section := userMemorySection(t, snapshot)
	entries := worldStateObjectSlice(section["entries"])
	encoded, err := json.Marshal(section)
	if err != nil {
		t.Fatal(err)
	}
	if total, ok := numericValue(section["total"]); !ok || total != storage.UserMemoryLimit ||
		section["truncated"] != true || len(entries) == 0 || len(entries) >= storage.UserMemoryLimit {
		t.Fatalf("budgeted user_memory metadata=%#v included=%d", section, len(entries))
	}
	if runes := utf8.RuneCount(encoded); runes > contextUserMemoryRuneBudget {
		t.Fatalf("user_memory section=%d runes, want <=%d", runes, contextUserMemoryRuneBudget)
	}
	for _, entry := range entries {
		if utf8.RuneCountInString(entry["statement"].(string)) != storage.UserMemoryStatementRuneLimit {
			t.Fatal("budget truncation must never split a memory statement")
		}
	}
}

func TestUserMemoryWorldStateSupportsOldReferencesAndBuildPriority(t *testing.T) {
	t.Parallel()
	if worldStateSchemaVersion != 1 {
		t.Fatalf("adding user_memory must not bump WorldState schema version: %d", worldStateSchemaVersion)
	}
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_memory_build")
	manager := NewContextManager(database)
	first, err := manager.Build(t.Context(), "draft_memory_build")
	if err != nil {
		t.Fatal(err)
	}
	oldBase := first.Checkpoint.BaseSnapshot
	oldSections, ok := oldBase["sections"].(map[string]any)
	if !ok {
		t.Fatalf("checkpoint sections=%#v", oldBase["sections"])
	}
	delete(oldSections, "user_memory")
	oldSnapshot, err := WorldStateSnapshotFromMap(oldBase)
	if err != nil {
		t.Fatal(err)
	}
	oldHash, err := oldSnapshot.Hash()
	if err != nil {
		t.Fatal(err)
	}
	oldCheckpoint := first.Checkpoint
	oldCheckpoint.BaseSnapshot = oldBase
	oldCheckpoint.BaseSnapshotHash = oldHash
	if err := manager.persistCheckpoint(t.Context(), oldCheckpoint); err != nil {
		t.Fatal(err)
	}

	insertAgentMessage(t, database, "draft_memory_build", "message_memory_build", "以后成片节奏都快一点")
	applyUserMemories(t, database, []reducer.UserMemoryRow{{
		Key: "pacing", Kind: "preference", Statement: "成片节奏偏快",
		EvidenceKind: storage.UserMemoryEvidenceMessage,
		EvidenceID:   "message_memory_build", SourceDraftID: "draft_memory_build",
	}}, nil)
	second, err := manager.Build(t.Context(), "draft_memory_build")
	if err != nil {
		t.Fatal(err)
	}
	if second.Manifest.WindowID != first.Manifest.WindowID || second.Manifest.WindowNumber != 1 ||
		!second.Manifest.HasWorldStatePatch || second.Manifest.ReferenceHash != oldHash ||
		second.Manifest.ReferenceHash == second.Manifest.CurrentHash {
		t.Fatalf("old checkpoint was not updated incrementally: first=%#v second=%#v", first.Manifest, second.Manifest)
	}
	if !strings.Contains(joinMessageContent(second.Messages), `"user_memory":{"entries":[{"key":"pacing"`) {
		t.Fatalf("rendered production context lost memory patch: %#v", second.Messages)
	}
	base, err := WorldStateSnapshotFromMap(second.Checkpoint.BaseSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := base.MergePatchTo(second.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	baseMap, _ := base.Map()
	currentMap, _ := second.Snapshot.Map()
	if reconstructed := applyMergePatch(baseMap, patch); !reflect.DeepEqual(reconstructed, currentMap) {
		t.Fatalf("memory patch cannot reconstruct current snapshot: patch=%#v", patch)
	}

	contextText, err := NewContextBuilder(database).Build(t.Context(), "draft_memory_build")
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"\"user_memory\"", "跨草稿", "当前用户指令冲突", "本回合指令为准", "memory.update"} {
		if !strings.Contains(contextText, fragment) {
			t.Fatalf("context build lost priority fragment %q", fragment)
		}
	}
}

func applyUserMemories(
	t *testing.T,
	database *storage.DB,
	upserts []reducer.UserMemoryRow,
	removeKeys []string,
) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{
			UserMemoryUpserts: upserts, UserMemoryRemoveKeys: removeKeys,
		},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("apply user memories result=%#v err=%v", result, err)
	}
}

func userMemorySection(t *testing.T, snapshot WorldStateSnapshot) map[string]any {
	t.Helper()
	section, ok := snapshot.Sections["user_memory"].(map[string]any)
	if !ok {
		t.Fatalf("user_memory section=%#v", snapshot.Sections["user_memory"])
	}
	return section
}
