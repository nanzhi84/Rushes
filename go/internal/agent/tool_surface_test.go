package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func seedSurfaceAsset(t *testing.T, service *Service, draftID string) {
	t.Helper()
	if _, err := service.database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES('asset_surface', 'reference', '/tmp/surface.mp4', 'video',
			'local_path', 'surface.mp4', 'surface', 1, '{}', 'ready', 'ready', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.database.Write().ExecContext(t.Context(), `
		INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
		VALUES(?, 'asset_surface', 'Broll', '2026-01-01T00:00:00Z')`, draftID); err != nil {
		t.Fatal(err)
	}
}

func seedSurfaceTranscript(t *testing.T, service *Service) {
	t.Helper()
	if _, err := service.database.Write().ExecContext(t.Context(), `
		INSERT INTO transcripts(
			transcript_id,asset_id,provider_id,raw_preserved,utterances_json,vad_segments_json
		) VALUES(
			'transcript_surface','asset_surface','fixture',0,
			'[{"utterance_id":"surface_u1","source_start_frame":0,"source_end_frame":30,"text":"测试口播"}]',
			'[]'
		)`); err != nil {
		t.Fatal(err)
	}
}

func containsName(names []string, target string) bool {
	for _, name := range names {
		if name == target {
			return true
		}
	}
	return false
}

type deterministicSchemaSpy struct {
	bound [][]string
}

func (spy *deterministicSchemaSpy) WithTools(infos []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Name)
	}
	spy.bound = append(spy.bound, names)
	return spy, nil
}

func (spy *deterministicSchemaSpy) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("ok", nil), nil
}

func (spy *deterministicSchemaSpy) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("ok", nil)}), nil
}

func TestDeterministicSchemaBindingUsesOnlyTranscriptDisclosures(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_schema_binding")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	spy := &deterministicSchemaSpy{}
	loader := &deterministicToolSchemaModel{inner: spy, registry: service.tools}
	ctx := rushestools.WithDraftID(t.Context(), "draft_schema_binding")
	ctx = withToolDisclosureSession(ctx)
	lease := newTimelineEditLeaseSession(database, "draft_schema_binding", "turn-schema-binding", func(error) {})
	t.Cleanup(lease.close)
	ctx = withTimelineEditLeaseSession(ctx, lease)

	if _, _, err := loader.bind(ctx, []*schema.Message{schema.UserMessage("剪辑并导出")}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(spy.bound[0], []string{"tool.load"}) {
		t.Fatalf("initial bound=%v", spy.bound[0])
	}
	if session := toolDisclosureSessionFromContext(ctx); len(session.snapshot()) != 0 {
		t.Fatalf("user text must not disclose schemas: %v", session.snapshot())
	}

	loaded := schema.ToolMessage(
		`{"status":"succeeded","loaded_names":["timeline.insert","shot.search"],"already_loaded":[],"not_loadable":[]}`,
		"load-1", schema.WithToolName("tool.load"),
	)
	if _, _, err := loader.bind(ctx, []*schema.Message{schema.UserMessage("任意文本"), loaded}); err != nil {
		t.Fatal(err)
	}
	want := []string{"tool.load", "shot.search", "timeline.insert"}
	if !reflect.DeepEqual(spy.bound[1], want) {
		t.Fatalf("loaded bound=%v want=%v", spy.bound[1], want)
	}
	secondLoad := schema.ToolMessage(
		`{"status":"succeeded","loaded_names":["timeline.update","timeline.insert"],"already_loaded":["shot.search"],"not_loadable":[]}`,
		"load-2", schema.WithToolName("tool.load"),
	)
	if _, _, err := loader.bind(ctx, []*schema.Message{loaded, secondLoad}); err != nil {
		t.Fatal(err)
	}
	want = []string{"tool.load", "shot.search", "timeline.insert", "timeline.update"}
	if !reflect.DeepEqual(spy.bound[2], want) {
		t.Fatalf("additive deduplicated bound=%v want=%v", spy.bound[2], want)
	}
	if _, _, err := loader.bind(ctx, []*schema.Message{schema.UserMessage("压缩后重新开始")}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(spy.bound[3], []string{"tool.load"}) {
		t.Fatalf("compacted transcript must require reload: %v", spy.bound[3])
	}
}

func TestLoadedModelActionNamesIgnoreFailedUnknownAndHarness(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_loaded_names")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	messages := []*schema.Message{
		schema.ToolMessage(`{"status":"failed","loaded_names":["timeline.insert"]}`, "bad", schema.WithToolName("tool.load")),
		schema.ToolMessage(`{"status":"succeeded","loaded_names":["timeline.update","timeline.check","missing"],"already_loaded":["speech.search"]}`, "ok", schema.WithToolName("tool.load")),
	}
	want := []string{"speech.search", "timeline.update"}
	if got := loadedModelActionNames(messages, service.tools); !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded=%v want=%v", got, want)
	}
}

func TestToolLoadIsExactIdempotentAndDoesNotAcquireLease(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_tool_load")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), "draft_tool_load")
	ctx = withToolDisclosureSession(ctx)
	lease := newTimelineEditLeaseSession(database, "draft_tool_load", "turn-load", func(error) {})
	ctx = withTimelineEditLeaseSession(ctx, lease)
	t.Cleanup(lease.close)

	first, err := executeToolLoad(ctx, service.tools, rushestools.ToolLoadInput{
		ToolNames: []string{"timeline.insert", "timeline.check", "missing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.LoadedNames, []string{"timeline.insert"}) ||
		!reflect.DeepEqual(first.NotLoadable, []string{"timeline.check", "missing"}) {
		t.Fatalf("first=%+v", first)
	}
	second, err := executeToolLoad(ctx, service.tools, rushestools.ToolLoadInput{ToolNames: []string{"timeline.insert"}})
	if err != nil || !reflect.DeepEqual(second.AlreadyLoaded, []string{"timeline.insert"}) {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	if lease.activeTurnID() != "" {
		t.Fatalf("tool.load acquired edit lease: %s", lease.activeTurnID())
	}
}

func TestToolLoadAdmissionFailureIsRejectedInReceiptAndPersistedTrace(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_tool_load_rejection"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	spec, exists := service.tools.Spec("tool.load")
	if !exists {
		t.Fatal("tool.load missing")
	}
	invokable := spec.Implementation.(einotool.InvokableTool)
	endpoint := newToolRecoveryMiddleware(
		testRetrySafe(t), service.tools.ModelReceiptPolicy,
	).Invokable(func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
		result, runErr := invokable.InvokableRun(ctx, input.Arguments)
		if runErr != nil {
			return nil, runErr
		}
		return &compose.ToolOutput{Result: result}, nil
	})
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	ctx = withToolDisclosureSession(ctx)
	ctx = rushestools.WithReporter(ctx, service.toolReporter(ctx, draftID))
	output, err := endpoint(ctx, &compose.ToolInput{
		Name: "tool.load", CallID: "bad_load", Arguments: `{"tool_names":[]}`,
	})
	if err != nil || output == nil {
		t.Fatalf("output=%#v err=%v", output, err)
	}
	var receipt map[string]any
	if json.Unmarshal([]byte(output.Result), &receipt) != nil || receipt["status"] != "rejected" {
		t.Fatalf("tool.load receipt=%s", output.Result)
	}
	messages, err := storage.ListMessages(t.Context(), database.Read(), draftID, 10)
	if err != nil || len(messages) != 1 || messages[0].Kind != "tool" ||
		!strings.Contains(messages[0].Content, `"status":"rejected"`) {
		t.Fatalf("tool.load persisted trace=%#v err=%v", messages, err)
	}
}

func TestToolDisclosureSessionMeasuresOnlyFirstActionAfterNewLoad(t *testing.T) {
	session := &toolDisclosureSession{loaded: map[string]struct{}{}}
	loadedAt := time.Unix(100, 0)
	session.recordLoadCompleted(loadedAt)
	if elapsed, ok := session.takeFirstActionRoundtrip(loadedAt.Add(125 * time.Millisecond)); !ok || elapsed != 125*time.Millisecond {
		t.Fatalf("first action elapsed=%s ok=%v", elapsed, ok)
	}
	if elapsed, ok := session.takeFirstActionRoundtrip(loadedAt.Add(time.Second)); ok || elapsed != 0 {
		t.Fatalf("same load measured twice: elapsed=%s ok=%v", elapsed, ok)
	}
}
