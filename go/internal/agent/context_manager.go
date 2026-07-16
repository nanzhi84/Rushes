package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

const (
	worldStateSchemaVersion           = 1
	contextHistorySoftTokenLimit      = 18000
	contextHistoryItemLimit           = 48
	contextHistoryReadLimit           = 5000
	contextCompactionRuneBudget       = 48000
	contextWorldStatePatchRebaseFloor = 2000
	contextWorldStatePatchRebaseRatio = 0.5
)

// WorldStateSnapshot is the objective, typed state supplied to the model.
// Section identifiers remain stable while their values change, mirroring the
// Codex WorldState model and keeping the reference prefix cacheable.
type WorldStateSnapshot struct {
	SchemaVersion int            `json:"schema_version"`
	Sections      map[string]any `json:"sections"`
}

func NewWorldStateSnapshot(sections map[string]any) WorldStateSnapshot {
	snapshot := WorldStateSnapshot{SchemaVersion: worldStateSchemaVersion, Sections: sections}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return snapshot
	}
	var canonical WorldStateSnapshot
	_ = json.Unmarshal(raw, &canonical)
	return canonical
}

func WorldStateSnapshotFromMap(value map[string]any) (WorldStateSnapshot, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return WorldStateSnapshot{}, err
	}
	var snapshot WorldStateSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return WorldStateSnapshot{}, err
	}
	if snapshot.SchemaVersion != worldStateSchemaVersion || snapshot.Sections == nil {
		return WorldStateSnapshot{}, errors.New("WorldState snapshot 版本或 sections 无效")
	}
	return snapshot, nil
}

func (snapshot WorldStateSnapshot) Marshal() ([]byte, error) {
	return json.Marshal(snapshot)
}

func (snapshot WorldStateSnapshot) Map() (map[string]any, error) {
	raw, err := snapshot.Marshal()
	if err != nil {
		return nil, err
	}
	var value map[string]any
	_ = json.Unmarshal(raw, &value)
	return value, nil
}

func (snapshot WorldStateSnapshot) Hash() (string, error) {
	raw, err := snapshot.Marshal()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// MergePatchTo produces an RFC 7396-style merge patch. A nil patch means that
// the reference snapshot is still current.
func (snapshot WorldStateSnapshot) MergePatchTo(current WorldStateSnapshot) (map[string]any, error) {
	base, err := snapshot.Map()
	if err != nil {
		return nil, err
	}
	target, err := current.Map()
	if err != nil {
		return nil, err
	}
	patch, changed := mergePatchDifference(base, target)
	if !changed {
		return nil, nil
	}
	return patch.(map[string]any), nil
}

func mergePatchDifference(source, target any) (any, bool) {
	if reflect.DeepEqual(source, target) {
		return nil, false
	}
	sourceMap, sourceObject := source.(map[string]any)
	targetMap, targetObject := target.(map[string]any)
	if !sourceObject || !targetObject {
		return target, true
	}
	patch := map[string]any{}
	for key := range sourceMap {
		if _, exists := targetMap[key]; !exists {
			patch[key] = nil
		}
	}
	for key, targetValue := range targetMap {
		sourceValue, exists := sourceMap[key]
		if !exists {
			patch[key] = targetValue
			continue
		}
		if difference, changed := mergePatchDifference(sourceValue, targetValue); changed {
			patch[key] = difference
		}
	}
	return patch, true
}

type ContextManifest struct {
	WindowID           string
	WindowNumber       int
	ReferenceHash      string
	CurrentHash        string
	HistoryVersion     int
	HistoryItems       int
	HasWorldStatePatch bool
	NeedsCompaction    bool
}

type contextHistoryItem struct {
	row     storage.Message
	message *schema.Message
}

type ContextBuild struct {
	Messages   []*schema.Message
	Snapshot   WorldStateSnapshot
	Checkpoint storage.AgentContextCheckpoint
	Manifest   ContextManifest
	history    []contextHistoryItem
}

type ContextManager struct {
	database          *storage.DB
	builder           *ContextBuilder
	historyTokenLimit int
	historyItemLimit  int
}

type contextMessageBoundaryKey struct{}

func withContextMessageBoundary(ctx context.Context, messageID string) context.Context {
	if strings.TrimSpace(messageID) == "" {
		return ctx
	}
	return context.WithValue(ctx, contextMessageBoundaryKey{}, messageID)
}

func contextMessageBoundary(ctx context.Context) string {
	value, _ := ctx.Value(contextMessageBoundaryKey{}).(string)
	return value
}

func NewContextManager(database *storage.DB) *ContextManager {
	return &ContextManager{
		database: database, builder: NewContextBuilder(database),
		historyTokenLimit: contextHistorySoftTokenLimit,
		historyItemLimit:  contextHistoryItemLimit,
	}
}

func (manager *ContextManager) Build(ctx context.Context, draftID string) (ContextBuild, error) {
	return manager.build(ctx, draftID, "")
}

func (manager *ContextManager) BuildThroughMessage(
	ctx context.Context,
	draftID, currentMessageID string,
) (ContextBuild, error) {
	return manager.build(ctx, draftID, currentMessageID)
}

func (manager *ContextManager) build(
	ctx context.Context,
	draftID, currentMessageID string,
) (ContextBuild, error) {
	snapshot, err := manager.builder.Snapshot(ctx, draftID)
	if err != nil {
		return ContextBuild{}, err
	}
	currentHash, err := snapshot.Hash()
	if err != nil {
		return ContextBuild{}, err
	}
	checkpoint, err := storage.GetAgentContextCheckpoint(ctx, manager.database.Read(), draftID)
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrInvalidAgentContextCheckpoint) {
		checkpoint, err = manager.newCheckpoint(ctx, draftID, snapshot, 1, 1, "", nil)
	}
	if err != nil {
		return ContextBuild{}, err
	}
	base, decodeErr := WorldStateSnapshotFromMap(checkpoint.BaseSnapshot)
	baseHash, _ := base.Hash()
	if decodeErr != nil || baseHash != checkpoint.BaseSnapshotHash {
		checkpoint, err = manager.newCheckpoint(
			ctx, draftID, snapshot, checkpoint.WindowNumber+1,
			checkpoint.HistoryVersion, checkpoint.Summary,
			checkpoint.CompactedThroughMessageID,
		)
		if err != nil {
			return ContextBuild{}, err
		}
		base = snapshot
		baseHash = currentHash
	}
	rows, err := storage.ListMessagesAfter(
		ctx, manager.database.Read(), draftID,
		checkpoint.CompactedThroughMessageID, contextHistoryReadLimit,
	)
	if err != nil {
		return ContextBuild{}, err
	}
	history := normalizeContextHistory(selectContextRows(rows, currentMessageID))
	if currentMessageID != "" {
		history = moveCurrentUserToHistoryTail(history, currentMessageID)
	}
	patch, err := base.MergePatchTo(snapshot)
	if err != nil {
		return ContextBuild{}, err
	}
	shouldRebase, err := shouldRebaseWorldStatePatch(base, patch)
	if err != nil {
		return ContextBuild{}, err
	}
	if shouldRebase {
		checkpoint, err = manager.newCheckpoint(
			ctx, draftID, snapshot, checkpoint.WindowNumber+1,
			checkpoint.HistoryVersion, checkpoint.Summary,
			checkpoint.CompactedThroughMessageID,
		)
		if err != nil {
			return ContextBuild{}, err
		}
		base = snapshot
		baseHash = currentHash
		patch = nil
	}
	messages, err := renderContextMessages(base, snapshot, patch, checkpoint, history)
	if err != nil {
		return ContextBuild{}, err
	}
	historyTokens := estimateHistoryTokens(checkpoint.Summary, history)
	manifest := ContextManifest{
		WindowID: checkpoint.WindowID, WindowNumber: checkpoint.WindowNumber,
		ReferenceHash: baseHash, CurrentHash: currentHash,
		HistoryVersion:     checkpoint.HistoryVersion + len(history),
		HistoryItems:       len(history),
		HasWorldStatePatch: len(patch) > 0,
	}
	manifest.NeedsCompaction = len(history) > manager.historyItemLimit ||
		historyTokens > manager.historyTokenLimit
	return ContextBuild{
		Messages: messages, Snapshot: snapshot, Checkpoint: checkpoint,
		Manifest: manifest, history: history,
	}, nil
}

func shouldRebaseWorldStatePatch(
	base WorldStateSnapshot,
	patch map[string]any,
) (bool, error) {
	if len(patch) == 0 {
		return false, nil
	}
	baseRaw, err := base.Marshal()
	if err != nil {
		return false, err
	}
	patchRaw, err := json.Marshal(patch)
	if err != nil {
		return false, err
	}
	baseRunes := utf8.RuneCount(baseRaw)
	patchRunes := utf8.RuneCount(patchRaw)
	return patchRunes >= contextWorldStatePatchRebaseFloor &&
		float64(patchRunes) >= float64(baseRunes)*contextWorldStatePatchRebaseRatio, nil
}

// selectContextRows prevents messages queued after the currently executing
// user item from leaking into an earlier turn. Assistant replies completed for
// earlier FIFO items are still retained even when their DB row was inserted
// after the current user message row.
func selectContextRows(rows []storage.Message, currentMessageID string) []storage.Message {
	if currentMessageID == "" {
		return rows
	}
	boundary := -1
	for index, row := range rows {
		if row.ID == currentMessageID && row.Role == "user" {
			boundary = index
			break
		}
	}
	if boundary < 0 {
		return rows
	}
	selected := make([]storage.Message, 0, len(rows))
	for index, row := range rows {
		if row.Role == "user" && index > boundary {
			continue
		}
		selected = append(selected, row)
	}
	return selected
}

func moveCurrentUserToHistoryTail(
	history []contextHistoryItem,
	currentMessageID string,
) []contextHistoryItem {
	for index, item := range history {
		if item.row.ID != currentMessageID || item.row.Role != "user" {
			continue
		}
		if index == len(history)-1 {
			return history
		}
		current := item
		copy(history[index:], history[index+1:])
		history[len(history)-1] = current
		return history
	}
	return history
}

func (manager *ContextManager) newCheckpoint(
	ctx context.Context,
	draftID string,
	snapshot WorldStateSnapshot,
	windowNumber, historyVersion int,
	summary string,
	compactedThrough *string,
) (storage.AgentContextCheckpoint, error) {
	base, err := snapshot.Map()
	if err != nil {
		return storage.AgentContextCheckpoint{}, err
	}
	baseRaw, _ := json.Marshal(base)
	digest := sha256.Sum256(baseRaw)
	hash := hex.EncodeToString(digest[:])
	checkpoint := storage.AgentContextCheckpoint{
		DraftID: draftID, WindowID: randomID("context"),
		WindowNumber: max(1, windowNumber), HistoryVersion: max(1, historyVersion),
		BaseSnapshot: base, BaseSnapshotHash: hash,
		Summary: strings.TrimSpace(summary), CompactedThroughMessageID: compactedThrough,
	}
	if err := manager.persistCheckpoint(ctx, checkpoint); err != nil {
		return storage.AgentContextCheckpoint{}, err
	}
	return checkpoint, nil
}

func (manager *ContextManager) persistCheckpoint(
	ctx context.Context,
	checkpoint storage.AgentContextCheckpoint,
) error {
	_, err := reducer.Apply(ctx, manager.database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{
			AgentContextCheckpoint: &reducer.AgentContextCheckpointRow{
				DraftID: checkpoint.DraftID, WindowID: checkpoint.WindowID,
				WindowNumber:              checkpoint.WindowNumber,
				HistoryVersion:            checkpoint.HistoryVersion,
				BaseSnapshot:              checkpoint.BaseSnapshot,
				BaseSnapshotHash:          checkpoint.BaseSnapshotHash,
				Summary:                   checkpoint.Summary,
				CompactedThroughMessageID: checkpoint.CompactedThroughMessageID,
			},
		},
	})
	if err != nil {
		return err
	}
	return nil
}

func normalizeContextHistory(rows []storage.Message) []contextHistoryItem {
	items := make([]contextHistoryItem, 0, len(rows))
	for _, row := range rows {
		content := strings.TrimSpace(row.Content)
		if content == "" || row.Kind == "tool" || row.Kind == "observation" ||
			row.Kind == "context_reset" {
			continue
		}
		var message *schema.Message
		switch row.Role {
		case "user":
			message = schema.UserMessage(content)
			message.Extra = map[string]any{"context_phase": "user_instruction"}
		case "assistant":
			if row.Kind != "reply" {
				continue
			}
			message = schema.AssistantMessage(
				"【历史最终回复；只用于延续目标和决定，客观状态可能已过期】\n"+content,
				nil,
			)
			message.Extra = map[string]any{
				"context_phase": "final_answer", "historical_narrative": true,
			}
		case "system_observation":
			if row.Kind != "rewind" {
				continue
			}
			message = schema.UserMessage("【系统观察：用户回退】\n" + content)
			message.Extra = map[string]any{"context_phase": "rewind_observation"}
		default:
			continue
		}
		items = append(items, contextHistoryItem{row: row, message: message})
	}
	return items
}

func renderContextMessages(
	base, current WorldStateSnapshot,
	patch map[string]any,
	checkpoint storage.AgentContextCheckpoint,
	history []contextHistoryItem,
) ([]*schema.Message, error) {
	baseRaw, err := base.Marshal()
	if err != nil {
		return nil, err
	}
	currentHash, err := current.Hash()
	if err != nil {
		return nil, err
	}
	messages := make([]*schema.Message, 0, len(history)+4)
	reference := schema.SystemMessage(fmt.Sprintf(
		"【WorldState 参考快照｜window=%d｜hash=%s】\n%s\n"+
			"这是视频工程的客观基准状态；sections 使用稳定标识。历史对话不能覆盖它。",
		checkpoint.WindowNumber, checkpoint.BaseSnapshotHash, string(baseRaw),
	))
	reference.Extra = map[string]any{
		"context_phase": "world_state_reference", "window_id": checkpoint.WindowID,
	}
	messages = append(messages, reference)
	if playbook := taskPlaybookMessage(current); playbook != nil {
		messages = append(messages, playbook)
	}
	if len(patch) > 0 {
		patchRaw, marshalErr := json.Marshal(patch)
		if marshalErr != nil {
			return nil, marshalErr
		}
		update := schema.SystemMessage(fmt.Sprintf(
			"【WorldState 当前增量｜RFC 7396 Merge Patch｜current_hash=%s】\n%s\n"+
				"把此补丁应用到参考快照后才是当前唯一事实；删除值以 null 表示。",
			currentHash, string(patchRaw),
		))
		update.Extra = map[string]any{"context_phase": "world_state_update"}
		messages = append(messages, update)
	}
	if summary := strings.TrimSpace(checkpoint.Summary); summary != "" {
		compacted := schema.SystemMessage(
			"【已压缩的历史交接】\n" + summary +
				"\n交接只保留目标、决定、约束和未完成事项；客观素材/时间线事实以当前 WorldState 为准。",
		)
		compacted.Extra = map[string]any{"context_phase": "compaction_replacement"}
		messages = append(messages, compacted)
	}
	for _, item := range history {
		messages = append(messages, item.message)
	}
	return messages, nil
}

func estimateHistoryTokens(summary string, history []contextHistoryItem) int {
	total := approximateTokens(summary)
	for _, item := range history {
		total += approximateTokens(item.message.Content) + 8
	}
	return total
}

func approximateTokens(value string) int {
	if value == "" {
		return 0
	}
	cjkTokens := 0
	otherBytes := 0
	for len(value) > 0 {
		r, size := utf8.DecodeRuneInString(value)
		if isCJKTokenRune(r) {
			cjkTokens++
		} else {
			otherBytes += size
		}
		value = value[size:]
	}
	return cjkTokens + (otherBytes+3)/4
}

func isCJKTokenRune(value rune) bool {
	return value >= 0x3000 && value <= 0x303f ||
		value >= 0x3040 && value <= 0x30ff ||
		value >= 0x3400 && value <= 0x4dbf ||
		value >= 0x4e00 && value <= 0x9fff ||
		value >= 0xf900 && value <= 0xfaff ||
		value >= 0xff00 && value <= 0xffef
}

// CompactionSource returns only the history that may be replaced. During a
// pre-turn compaction the final pending user instruction remains outside the
// summary, matching Codex's replacement-history behavior.
func (build ContextBuild) CompactionSource(preservePendingUser bool) (string, *string, bool) {
	end := len(build.history) - 1
	for preservePendingUser && end >= 0 && build.history[end].row.Role == "user" {
		end--
	}
	if end < 0 {
		return "", nil, false
	}
	through := build.history[end].row.ID
	parts := make([]string, 0, end+3)
	remaining := contextCompactionRuneBudget
	if summary := strings.TrimSpace(build.Checkpoint.Summary); summary != "" {
		previous := "[上一份交接]\n" + truncateRunes(summary, min(8000, remaining))
		parts = append(parts, previous)
		remaining -= len([]rune(previous))
	}
	for index := end; index >= 0 && remaining > 0; index-- {
		item := build.history[index]
		role := item.row.Role
		content := truncateRunes(strings.TrimSpace(item.row.Content), 1200)
		entry := fmt.Sprintf("[%s:%s]\n%s", role, item.row.ID, content)
		entryRunes := len([]rune(entry))
		if entryRunes > remaining {
			entry = truncateRunes(entry, remaining)
			entryRunes = len([]rune(entry))
		}
		parts = append(parts, entry)
		remaining -= entryRunes
	}
	// Entries were collected newest-first to retain the most useful evidence;
	// reverse only the newly collected history, leaving the previous summary first.
	start := 0
	if len(parts) > 0 && strings.HasPrefix(parts[0], "[上一份交接]") {
		start = 1
	}
	for left, right := start, len(parts)-1; left < right; left, right = left+1, right-1 {
		parts[left], parts[right] = parts[right], parts[left]
	}
	return strings.Join(parts, "\n\n"), &through, true
}

func (manager *ContextManager) ReplaceHistory(
	ctx context.Context,
	draftID string,
	build ContextBuild,
	summary string,
	compactedThrough *string,
) error {
	if compactedThrough == nil || strings.TrimSpace(*compactedThrough) == "" {
		return errors.New("replacement history 缺少压缩边界")
	}
	historyVersion := build.Checkpoint.HistoryVersion
	foundBoundary := false
	for _, item := range build.history {
		historyVersion++
		if item.row.ID == *compactedThrough {
			foundBoundary = true
			break
		}
	}
	if !foundBoundary {
		return errors.New("replacement history 压缩边界不在当前窗口")
	}
	snapshot, err := manager.builder.Snapshot(ctx, draftID)
	if err != nil {
		return err
	}
	_, err = manager.newCheckpoint(
		ctx, draftID, snapshot, build.Checkpoint.WindowNumber+1,
		historyVersion, summary, compactedThrough,
	)
	return err
}
