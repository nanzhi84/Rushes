package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

type currentTimelineViewCaptureModel struct {
	calls [][]*schema.Message
}

func (capture *currentTimelineViewCaptureModel) WithTools(
	[]*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	return capture, nil
}

func (capture *currentTimelineViewCaptureModel) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	capture.calls = append(capture.calls, append([]*schema.Message(nil), messages...))
	return schema.AssistantMessage("ok", nil), nil
}

func (capture *currentTimelineViewCaptureModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	response, err := capture.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{response}), nil
}

func TestDynamicProviderGetsExactlyOneFreshCurrentTimelineView(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft-current-view-refresh"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	baseVersion := 0
	first := currentTimelineViewEvent(draftID, 1, "manual", "clip-a", -3)
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{first}, reducer.Options{
		Actor: contracts.ActorUser, BaseVersion: &baseVersion,
		TimelineWriteAdmission: &reducer.TimelineWriteAdmission{Origin: "manual"},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed v1 result=%#v err=%v", result, err)
	}

	turnCtx, cancelTurn := context.WithCancelCause(t.Context())
	session := newTimelineEditLeaseSession(database, draftID, "turn-current-view", cancelTurn)
	t.Cleanup(session.close)
	turnCtx = rushestools.WithDraftID(turnCtx, draftID)
	turnCtx = rushestools.WithTurnIdentity(turnCtx, "turn-current-view", "message-current-view")
	turnCtx = withModelToolSurfaceSession(turnCtx)
	turnCtx = withTimelineEditLeaseSession(turnCtx, session)
	turnCtx = rushestools.WithTimelineWriteAdmission(
		turnCtx, "turn-current-view", session.token, session.markLost,
	)
	capture := &currentTimelineViewCaptureModel{}
	surface := &dynamicToolSurfaceModel{inner: capture, registry: service.tools}
	prompt := []*schema.Message{schema.UserMessage("只修改时间线片段音量")}
	if _, err := surface.Generate(turnCtx, prompt); err != nil {
		t.Fatal(err)
	}
	if session.activeTurnID() != "turn-current-view" {
		t.Fatal("timeline edit surface did not lazily acquire the lease")
	}

	draft, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	second := currentTimelineViewEvent(draftID, 2, "agent", "clip-a", -9)
	result, err = reducer.Apply(t.Context(), database, []contracts.Event{second}, reducer.Options{
		Actor: contracts.ActorAgent, BaseVersion: &draft.StateVersion,
		TimelineWriteAdmission: &reducer.TimelineWriteAdmission{
			Origin: "agent", TurnID: "turn-current-view", LeaseToken: session.token,
			Now: time.Now().UTC(),
		},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("commit v2 result=%#v err=%v", result, err)
	}
	if _, err := surface.Generate(turnCtx, prompt); err != nil {
		t.Fatal(err)
	}

	if len(capture.calls) != 2 {
		t.Fatalf("provider calls=%d", len(capture.calls))
	}
	views := make([]map[string]any, 0, 2)
	for callIndex, messages := range capture.calls {
		if messages[len(messages)-1].Role != schema.User {
			t.Fatalf("provider call %d tail role=%s want=user", callIndex+1, messages[len(messages)-1].Role)
		}
		var viewMessages []*schema.Message
		for _, message := range messages {
			if phase, _ := message.Extra["context_phase"].(string); phase == currentTimelineViewContextPhase {
				viewMessages = append(viewMessages, message)
			}
		}
		if len(viewMessages) != 1 {
			t.Fatalf("provider call %d current views=%d", callIndex+1, len(viewMessages))
		}
		views = append(views, decodeCurrentTimelineViewMessage(t, viewMessages[0].Content))
	}
	if views[0]["version"] != float64(1) || views[1]["version"] != float64(2) ||
		views[0]["timeline_id"] != draftID+":v1" || views[1]["timeline_id"] != draftID+":v2" {
		t.Fatalf("views did not advance N to N+1: %#v", views)
	}
	if views[0]["edit_lease_turn_id"] != "turn-current-view" ||
		views[1]["edit_lease_turn_id"] != "turn-current-view" {
		t.Fatalf("lease identity missing: %#v", views)
	}
	history := views[1]["recent_edit_history"].([]any)
	if len(history) != 2 {
		t.Fatalf("history=%#v", history)
	}
	manual := history[0].(map[string]any)
	agent := history[1].(map[string]any)
	if manual["actor"] != "user" || manual["origin"] != "manual" ||
		agent["actor"] != "agent" || agent["origin"] != "agent" {
		t.Fatalf("history lost stable actor/origin order: %#v", history)
	}
}

func TestRefreshCurrentTimelineViewPreservesConversationTail(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft-current-view-tail"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := withTestTurnLeaseSession(t, service, t.Context(), draftID)

	oldView := schema.SystemMessage("stale")
	oldView.Extra = map[string]any{"context_phase": currentTimelineViewContextPhase}
	tool := schema.ToolMessage("{}", "call-tail", schema.WithToolName("timeline.inspect"))
	input := []*schema.Message{
		schema.SystemMessage("core"), oldView, schema.UserMessage("edit"),
		schema.AssistantMessage("", nil), tool,
	}
	refreshed, err := refreshCurrentTimelineView(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshed) != len(input) || refreshed[len(refreshed)-1] != tool {
		t.Fatalf("conversation tail changed: %#v", refreshed)
	}
	wantRoles := []schema.RoleType{
		schema.System, schema.System, schema.User, schema.Assistant, schema.Tool,
	}
	viewCount := 0
	for index, message := range refreshed {
		if message.Role != wantRoles[index] {
			t.Fatalf("message %d role=%s want=%s", index, message.Role, wantRoles[index])
		}
		if phase, _ := message.Extra["context_phase"].(string); phase == currentTimelineViewContextPhase {
			viewCount++
			if index != 1 || message == oldView {
				t.Fatalf("fresh view index=%d message=%#v", index, message)
			}
		}
	}
	if viewCount != 1 {
		t.Fatalf("current views=%d", viewCount)
	}
}

func TestLongCurrentTimelineViewUsesDeterministicRelevantWindow(t *testing.T) {
	document := timeline.Empty("draft-current-view-long", 7)
	document.DurationFrames = 72 * 30
	for index := 0; index < 72; index++ {
		clipID := fmt.Sprintf("clip-%03d", index)
		clip := timeline.Clip{
			TimelineClipID: clipID, TrackID: "visual_base",
			AssetID: fmt.Sprintf("asset-%03d", index), AssetKind: "video", Role: "a_roll",
			TimelineStartFrame: index * 30, TimelineEndFrame: (index + 1) * 30,
			SourceStartFrame: index * 30, SourceEndFrame: (index + 1) * 30,
			PlaybackRate: 1,
		}
		if index == 37 {
			clip.FadeInFrames = 4
			clip.FadeOutFrames = 6
			clip.SubtitleStyle = "bold_bottom"
			clip.Effects = []map[string]any{{
				"kind": "color_grade", "preset": "warm",
			}}
			clip.Metadata = map[string]any{"editor_note": "保留这个语义字段"}
		}
		document.Tracks[0].Clips = append(document.Tracks[0].Clips, clip)
	}
	document.Tracks[4].Ducking = &timeline.TrackDucking{
		Enabled: true, DuckDB: -9, TriggerTracks: []string{"voiceover"},
	}
	batches := []storage.TimelineEditBatch{{
		Sequence: 9, AfterVersion: 7,
		AffectedRefs: []string{"timeline_clip_id:clip-037"},
	}}

	tracks, clips, compaction := buildCurrentTimelineContext(document, batches)
	if len(clips) != currentTimelineWindowClipLimit {
		t.Fatalf("included clips=%d want=%d", len(clips), currentTimelineWindowClipLimit)
	}
	if compaction == nil || compaction["mode"] != "compact_topology_relevant_window" ||
		compaction["strategy"] != "latest_affected_clip" ||
		compaction["total_clip_count"] != 72 ||
		compaction["included_clip_count"] != currentTimelineWindowClipLimit ||
		compaction["omitted_clip_count"] != 72-currentTimelineWindowClipLimit {
		t.Fatalf("compaction=%#v", compaction)
	}
	window := compaction["window"].(map[string]any)
	if window["start_frame"] == nil || window["end_frame"] == nil ||
		window["clip_order_start"] == nil || window["clip_order_end_exclusive"] == nil {
		t.Fatalf("window boundaries=%#v", window)
	}
	anchorRefs := window["anchor_clip_refs"].([]string)
	if len(anchorRefs) != 1 || anchorRefs[0] != "clip-037" {
		t.Fatalf("window anchor=%#v", anchorRefs)
	}
	omitted := compaction["omitted_ranges"].([]map[string]any)
	if len(omitted) != 2 || omitted[0]["position"] != "before_window" ||
		omitted[1]["position"] != "after_window" ||
		omitted[0]["clip_count"].(int)+omitted[1]["clip_count"].(int) != 72-len(clips) {
		t.Fatalf("omitted ranges=%#v", omitted)
	}
	if hint, _ := compaction["inspect_hint"].(string); !strings.Contains(hint, "timeline.inspect") {
		t.Fatalf("inspect hint=%q", hint)
	}

	flattened := flattenCurrentTimelineClips(tracks)
	wantFlat, _ := json.Marshal(clips)
	gotFlat, _ := json.Marshal(flattened)
	if string(gotFlat) != string(wantFlat) {
		t.Fatalf("root tracks/clips disagree\ntracks=%s\nclips=%s", gotFlat, wantFlat)
	}
	includedByTrack := 0
	for _, track := range tracks {
		includedByTrack += track["included_clip_count"].(int)
	}
	if includedByTrack != len(clips) || tracks[0]["clip_count"] != 72 ||
		tracks[0]["omitted_clip_count"] != 72-len(clips) {
		t.Fatalf("track topology=%#v", tracks[0])
	}
	if _, ok := tracks[4]["ducking"].(*timeline.TrackDucking); !ok {
		t.Fatalf("track ducking missing: %#v", tracks[4])
	}
	var anchor map[string]any
	for _, clip := range clips {
		if clip["timeline_clip_id"] == "clip-037" {
			anchor = clip
			break
		}
	}
	if anchor == nil || anchor["fade_in_frames"] != 4 || anchor["fade_out_frames"] != 6 ||
		anchor["subtitle_style"] != "bold_bottom" || anchor["effects"] == nil ||
		anchor["metadata"] == nil {
		t.Fatalf("relevant clip lost editable semantics: %#v", anchor)
	}

	secondTracks, secondClips, secondCompaction := buildCurrentTimelineContext(document, batches)
	firstJSON, _ := json.Marshal([]any{tracks, clips, compaction})
	secondJSON, _ := json.Marshal([]any{secondTracks, secondClips, secondCompaction})
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("long timeline view is not deterministic\nfirst=%s\nsecond=%s", firstJSON, secondJSON)
	}
}

func TestCurrentTimelineViewBindsPreviewToExactTimelineVersion(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft-current-view-preview-version"
	agenttest.CreateAgentDraft(t, database, draftID)
	baseVersion := 0
	first := currentTimelineViewEvent(draftID, 1, "manual", "clip-preview", -3)
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{first}, reducer.Options{
		Actor: contracts.ActorUser, BaseVersion: &baseVersion,
		TimelineWriteAdmission: &reducer.TimelineWriteAdmission{Origin: "manual"},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed v1 result=%#v err=%v", result, err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	hash := strings.Repeat("d", 64)
	if _, err := database.Write().ExecContext(t.Context(),
		"INSERT INTO objects(hash,rel_path,size,created_at) VALUES(?, ?, 1, ?)",
		hash, "objects/preview-version-1.mp4", now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO previews(preview_id,draft_id,timeline_version,object_hash,quality_json,created_at)
		VALUES('preview-version-1',?,1,?,'{}',?)`, draftID, hash, now,
	); err != nil {
		t.Fatal(err)
	}

	viewV1, err := buildCurrentTimelineView(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	preview, ok := viewV1["active_preview"].(map[string]any)
	if !ok || preview["preview_id"] != "preview-version-1" ||
		preview["timeline_id"] != draftID+":v1" || preview["timeline_version"] != 1 {
		t.Fatalf("v1 preview binding=%#v", viewV1["active_preview"])
	}

	draft, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	second := currentTimelineViewEvent(draftID, 2, "manual", "clip-preview", -6)
	result, err = reducer.Apply(t.Context(), database, []contracts.Event{second}, reducer.Options{
		Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion,
		TimelineWriteAdmission: &reducer.TimelineWriteAdmission{Origin: "manual"},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed v2 result=%#v err=%v", result, err)
	}
	viewV2, err := buildCurrentTimelineView(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	if viewV2["timeline_id"] != draftID+":v2" || viewV2["version"] != 2 ||
		viewV2["active_preview"] != nil {
		t.Fatalf("stale preview represented current v2: %#v", viewV2)
	}
}

func TestCurrentTimelineViewReadsOneCoherentSQLiteSnapshot(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft-current-view-snapshot"
	agenttest.CreateAgentDraft(t, database, draftID)
	baseVersion := 0
	first := currentTimelineViewEvent(draftID, 1, "manual", "clip-snapshot", -3)
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{first}, reducer.Options{
		Actor: contracts.ActorUser, BaseVersion: &baseVersion,
		TimelineWriteAdmission: &reducer.TimelineWriteAdmission{Origin: "manual"},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed v1 result=%#v err=%v", result, err)
	}

	// BEGIN DEFERRED lets the WAL writer advance after this reader establishes
	// its snapshot. The helper must nevertheless return a wholly-v1 view.
	connection, err := database.Read().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if _, err := connection.ExecContext(t.Context(), "BEGIN DEFERRED"); err != nil {
		t.Fatal(err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = connection.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if _, err := storage.GetDraft(t.Context(), connection, draftID); err != nil {
		t.Fatal(err)
	}

	draft, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	second := currentTimelineViewEvent(draftID, 2, "manual", "clip-snapshot", -9)
	result, err = reducer.Apply(t.Context(), database, []contracts.Event{second}, reducer.Options{
		Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion,
		TimelineWriteAdmission: &reducer.TimelineWriteAdmission{Origin: "manual"},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("commit v2 result=%#v err=%v", result, err)
	}

	pinned, err := buildCurrentTimelineViewFromQuery(
		t.Context(), connection, draftID, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if pinned["timeline_id"] != draftID+":v1" || pinned["version"] != 1 {
		t.Fatalf("pinned snapshot mixed timeline versions: %#v", pinned)
	}
	pinnedClips := pinned["clips"].([]map[string]any)
	if len(pinnedClips) != 1 || pinnedClips[0]["gain_db"] != float64(-3) {
		t.Fatalf("pinned snapshot mixed v2 document: %#v", pinnedClips)
	}
	if history := pinned["recent_edit_history"].([]map[string]any); len(history) != 1 || history[0]["after_version"] != 1 {
		t.Fatalf("pinned snapshot mixed v2 history: %#v", history)
	}
	if _, err := connection.ExecContext(t.Context(), "COMMIT"); err != nil {
		t.Fatal(err)
	}
	committed = true

	latest, err := buildCurrentTimelineView(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	if latest["timeline_id"] != draftID+":v2" || latest["version"] != 2 {
		t.Fatalf("latest view did not advance after snapshot: %#v", latest)
	}
	if history := latest["recent_edit_history"].([]map[string]any); len(history) != 2 {
		t.Fatalf("latest history=%#v", history)
	}
}

func TestCurrentTimelineViewFailsClosedWithoutAuthoritativeTurnContext(t *testing.T) {
	if _, err := generateWithCurrentTimelineView(t.Context(), nil, nil); err == nil {
		t.Fatal("nil provider was accepted")
	}
	if _, err := refreshCurrentTimelineView(t.Context(), nil); err == nil {
		t.Fatal("missing draft identity was accepted")
	}
	ctx := rushestools.WithDraftID(t.Context(), "draft-view-context")
	if _, err := refreshCurrentTimelineView(ctx, nil); err == nil {
		t.Fatal("missing lease session was accepted")
	}
	database := agenttest.AgentTestDatabase(t)
	mismatched := withTimelineEditLeaseSession(ctx, &timelineEditLeaseSession{
		database: database, draftID: "another-draft",
	})
	if _, err := refreshCurrentTimelineView(mismatched, nil); err == nil ||
		!strings.Contains(err.Error(), "草稿不匹配") {
		t.Fatalf("mismatched session err=%v", err)
	}
	if _, err := buildCurrentTimelineView(t.Context(), database, "missing-draft"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing draft err=%v", err)
	}

	const corruptDraftID = "draft-view-corrupt-pointer"
	agenttest.CreateAgentDraft(t, database, corruptDraftID)
	if _, err := database.Write().ExecContext(t.Context(), `
		UPDATE drafts SET timeline_current_version=99 WHERE draft_id=?`, corruptDraftID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := buildCurrentTimelineView(t.Context(), database, corruptDraftID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("corrupt timeline pointer err=%v", err)
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := buildCurrentTimelineView(cancelled, database, corruptDraftID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled snapshot err=%v", err)
	}
	if got := timelineEditHistoryKey(2, 3); got != "b00000000000000000002-o00000000000000000003" {
		t.Fatalf("history key=%q", got)
	}
}

func TestLongCurrentTimelineViewFallsBackToTailWithStableTieBreakers(t *testing.T) {
	document := timeline.Empty("draft-current-view-tail-window", 3)
	document.Tracks = document.Tracks[:3]
	document.Tracks[0].TrackID = "same-track"
	document.Tracks[1].TrackID = "same-track"
	document.Tracks[2].TrackID = "z-track"
	for trackIndex := range document.Tracks {
		count := 26
		if trackIndex == 2 {
			count = 4
		}
		for index := 0; index < count; index++ {
			// Duplicate adjacent IDs make the final stable clip-index tie breaker
			// observable; reversed IDs and duplicate track IDs exercise the other
			// deterministic ordering levels.
			clipID := fmt.Sprintf("clip-%02d", (count-1-index)/2)
			document.Tracks[trackIndex].Clips = append(document.Tracks[trackIndex].Clips, timeline.Clip{
				TimelineClipID: clipID, TrackID: document.Tracks[trackIndex].TrackID,
				AssetID: "asset", AssetKind: "video", Role: "a_roll",
				TimelineStartFrame: 0, TimelineEndFrame: 30,
				SourceStartFrame: 0, SourceEndFrame: 30, PlaybackRate: 1,
			})
		}
	}
	_, clips, compaction := buildCurrentTimelineContext(document, []storage.TimelineEditBatch{{
		AffectedRefs: []string{"asset_id:not-a-timeline-clip"},
	}})
	if len(clips) != currentTimelineWindowClipLimit ||
		compaction["strategy"] != "timeline_tail" {
		t.Fatalf("clips=%d compaction=%#v", len(clips), compaction)
	}
	omitted := compaction["omitted_ranges"].([]map[string]any)
	if len(omitted) != 1 || omitted[0]["position"] != "before_window" ||
		omitted[0]["clip_count"] != 32 {
		t.Fatalf("omitted=%#v", omitted)
	}
}

func currentTimelineViewEvent(
	draftID string,
	version int,
	origin, clipID string,
	gainDB float64,
) contracts.Event {
	timelineID := fmt.Sprintf("%s:v%d", draftID, version)
	return contracts.Event{
		Type: "TimelineVersionCreated", DraftID: draftID,
		Payload: map[string]any{
			"timeline_id": timelineID, "timeline_version": version,
			"patch_id": timelineID + ":patch", "edit_origin": origin,
			"edit_operations": []any{map[string]any{
				"kind": "adjust_gain", "timeline_clip_id": clipID, "gain_db": gainDB,
			}},
			"document_json": map[string]any{
				"timeline_id": timelineID, "draft_id": draftID, "version": version,
				"fps": 30, "duration_frames": 30,
				"tracks": []any{map[string]any{
					"track_id": "visual_base", "track_type": "primary_visual",
					"clips": []any{map[string]any{
						"timeline_clip_id": clipID, "track_id": "visual_base",
						"asset_id": "asset-a", "asset_kind": "video", "role": "a_roll",
						"timeline_start_frame": 0, "timeline_end_frame": 30,
						"source_start_frame": 0, "source_end_frame": 30,
						"playback_rate": 1, "gain_db": gainDB,
					}},
				}},
			},
		},
	}
}

func decodeCurrentTimelineViewMessage(t *testing.T, content string) map[string]any {
	t.Helper()
	start := strings.IndexByte(content, '{')
	end := strings.LastIndex(content, "\n这是当前时间线")
	if start < 0 || end <= start {
		t.Fatalf("invalid CurrentTimelineView message: %s", content)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(content[start:end]), &view); err != nil {
		t.Fatal(err)
	}
	return view
}
