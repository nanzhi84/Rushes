package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
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

func seedUnavailableSurfaceAsset(t *testing.T, service *Service, draftID string) {
	t.Helper()
	if _, err := service.database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES('asset_surface_unavailable', 'reference', '/tmp/surface-unavailable.mp4', 'video',
			'local_path', 'surface-unavailable.mp4', 'surface-unavailable', 1, '{}',
			'ready', 'pending', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.database.Write().ExecContext(t.Context(), `
		INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
		VALUES(?, 'asset_surface_unavailable', 'Broll', '2026-01-01T00:00:00Z')`, draftID); err != nil {
		t.Fatal(err)
	}
}

func setSurfaceTimelineState(t *testing.T, service *Service, draftID string, validated bool) {
	t.Helper()
	if _, err := service.database.Write().ExecContext(t.Context(), `
		UPDATE drafts SET timeline_current_version=1, timeline_validated=?
		WHERE draft_id=?`, validated, draftID); err != nil {
		t.Fatal(err)
	}
}

func seedSurfacePreview(t *testing.T, service *Service, draftID string) {
	t.Helper()
	if _, err := service.database.Write().ExecContext(t.Context(), `
		INSERT INTO objects(hash,rel_path,size,created_at)
		VALUES('surface_preview_hash','surface_preview.mp4',1,'2026-01-01T00:00:00Z');
		INSERT INTO previews(
			preview_id,draft_id,timeline_version,object_hash,quality_json,created_at
		) VALUES(
			'surface_preview',?,1,'surface_preview_hash','{}','2026-01-01T00:00:00Z'
		)`, draftID); err != nil {
		t.Fatal(err)
	}
}

func surfaceNames(specs []rushestools.Spec) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}

func TestDynamicModelToolSurfaceUsesStateIntentAndBudgets(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_dynamic_surface"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	assertSurface := func(prompt string, required, forbidden []string) []rushestools.Spec {
		t.Helper()
		specs, selectErr := selectModelToolSurface(ctx, service.tools, []*schema.Message{
			schema.UserMessage(prompt),
		})
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		names := surfaceNames(specs)
		for _, name := range required {
			if !containsName(names, name) {
				t.Errorf("%q surface=%v missing %s", prompt, names, name)
			}
		}
		for _, name := range forbidden {
			if containsName(names, name) {
				t.Errorf("%q surface=%v unexpectedly contains %s", prompt, names, name)
			}
		}
		metrics, metricErr := modelToolSchemaSizeFromTools(ctx, implementationsForSpecs(specs))
		if metricErr != nil {
			t.Fatal(metricErr)
		}
		if len(specs) > maxBoundModelTools || metrics.TotalRunes > maxBoundModelSchemaRunes {
			t.Fatalf("%q over budget: tools=%d runes=%d", prompt, len(specs), metrics.TotalRunes)
		}
		return specs
	}

	assertSurface(
		"请先看看有哪些素材",
		[]string{"asset.list_assets"},
		[]string{"timeline.update"},
	)

	seedSurfaceAsset(t, service, draftID)
	assertSurface(
		"读取口播台词和气口证据",
		[]string{
			"speech.transcribe", "audio.analyze_speech_pauses",
			"media.detect_shots", "shot.search",
		},
		[]string{
			"speech.search", "timeline.insert", "timeline.delete", "timeline.update",
			"timeline.split", "preview.generate",
		},
	)
	seedSurfaceTranscript(t, service)
	assertSurface(
		"读取口播台词和气口证据",
		[]string{"speech.transcribe", "speech.search", "audio.analyze_speech_pauses"},
		[]string{
			"timeline.insert", "timeline.delete", "timeline.update", "timeline.split",
			"preview.generate",
		},
	)
	assertSurface(
		"请组装初版时间线",
		[]string{"asset.list_assets", "shot.search"},
		[]string{"timeline.insert", "timeline.update"},
	)
	composeSpecs, err := selectModelToolSurface(ctx, service.tools, []*schema.Message{
		schema.UserMessage("请组装初版时间线"),
		schema.ToolMessage(
			`{"draft_id":"draft_dynamic_surface","assets":[{}]}`,
			"call_list_assets",
			schema.WithToolName("asset.list_assets"),
		),
		schema.ToolMessage(
			`{"shots":[{"shot_id":"shot-a"}]}`,
			"call_shot_search",
			schema.WithToolName("shot.search"),
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	if names := surfaceNames(composeSpecs); !containsName(names, "timeline.insert") {
		t.Fatalf("读取素材证据后未进入原子编辑阶段: %v", names)
	}

	setSurfaceTimelineState(t, service, draftID, false)
	assertSurface(
		"完成口播气口和重说清理",
		[]string{"speech.search", "shot.search", "media.detect_shots", "timeline.inspect"},
		[]string{"timeline.update"},
	)
	assertSurface(
		"验证时间线后渲染预览",
		[]string{"timeline.check", "timeline.inspect", "preview.generate"},
		[]string{"timeline.update"},
	)

	setSurfaceTimelineState(t, service, draftID, true)
	assertSurface(
		"渲染预览并导出最终 MP4",
		[]string{"preview.generate", "timeline.check"},
		[]string{"timeline.update"},
	)
	assertSurface(
		"记住我的长期偏好并更新计划",
		[]string{"memory.set", "plan.update"},
		[]string{
			"memory.remove", "interaction.confirm_action", "decision.answer",
			"timeline.update", "speech.search",
		},
	)
}

func TestControlToolSurfaceNarrowsToCurrentAction(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_control_surface"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	assertExact := func(messages []*schema.Message, want ...string) {
		t.Helper()
		specs, selectErr := selectModelToolSurface(ctx, service.tools, messages)
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		names := surfaceNames(specs)
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("surface=%v want=%v", names, want)
		}
	}

	assertExact(
		[]*schema.Message{schema.UserMessage("记住我偏好短视频并更新计划")},
		"memory.set", "plan.update",
	)
	assertExact(
		[]*schema.Message{schema.UserMessage("忘记我偏好短视频")},
		"memory.remove",
	)
	assertExact(
		[]*schema.Message{
			schema.UserMessage("忘记我偏好短视频"),
			schema.ToolMessage(
				`{"status":"failed","data":{"error_code":"confirmation_required"}}`,
				"call_memory_remove",
				schema.WithToolName("memory.remove"),
			),
		},
		"interaction.confirm_action",
	)

	snapshot := NewWorldStateSnapshot(map[string]any{
		"draft": map[string]any{"pending_decision_id": "decision_surface"},
	})
	raw, err := snapshot.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	worldState := schema.SystemMessage("【WorldState 参考快照】\n" + string(raw))
	worldState.Extra = map[string]any{"context_phase": "world_state_reference"}
	assertExact(
		[]*schema.Message{worldState, schema.UserMessage("我选择第一个选项并更新计划")},
		"decision.answer",
	)
}

func TestRippleEditForcesTimelineInspectBeforeMoreModelTools(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_ripple_observation_surface"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	setSurfaceTimelineState(t, service, draftID, false)
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	user := schema.UserMessage("根据节拍继续修改现有时间线")
	ripple := schema.ToolMessage(
		`{"status":"succeeded","data":{"timeline_id":"draft_ripple_observation_surface:v2",`+
			`"coordinate_effect":{`+
			`"duration_frames_before":1440,"duration_frames_after":1308,`+
			`"ripple_delta_frames":-132,"observation_required":true}}}`,
		"call_ripple",
		schema.WithToolName("timeline.update"),
	)

	messages := []*schema.Message{user, ripple}
	if !timelineObservationRequiredSinceLatestUser(messages) {
		t.Fatal("成功波纹编辑后未要求重新观察")
	}
	specs, err := selectModelToolSurface(ctx, service.tools, messages)
	if err != nil {
		t.Fatal(err)
	}
	if names := surfaceNames(specs); !reflect.DeepEqual(names, []string{"timeline.inspect"}) {
		t.Fatalf("ripple surface=%v want=[timeline.inspect]", names)
	}

	failedInspect := schema.ToolMessage(
		`{"status":"failed"}`, "call_failed_inspect",
		schema.WithToolName("timeline.inspect"),
	)
	if !timelineObservationRequiredSinceLatestUser(append(messages, failedInspect)) {
		t.Fatal("失败的 inspect 不得清除重新观察要求")
	}
	decisionContinuation := schema.UserMessage(
		"用户刚刚回答了你此前提出的选择题。这是同一条任务的继续，不是新的请求。",
	)
	if !timelineObservationRequiredSinceLatestUser(append(messages, decisionContinuation)) {
		t.Fatal("真实用户回答的同任务继续不得丢失重新观察要求")
	}

	staleInspect := schema.ToolMessage(
		`{"status":"succeeded","data":{"timeline_id":"draft_ripple_observation_surface:v1",`+
			`"is_current":false}}`, "call_stale_inspect",
		schema.WithToolName("timeline.inspect"),
	)
	if !timelineObservationRequiredSinceLatestUser(append(messages, staleInspect)) {
		t.Fatal("成功读取旧 timeline_id 不得清除最新版本观察要求")
	}
	invalidatedVersionNowStale := schema.ToolMessage(
		`{"status":"succeeded","data":{"timeline_exists":true,`+
			`"timeline_id":"draft_ripple_observation_surface:v2","is_current":false}}`,
		"call_invalidated_version_now_stale",
		schema.WithToolName("timeline.inspect"),
	)
	if !timelineObservationRequiredSinceLatestUser(
		append(messages, invalidatedVersionNowStale),
	) {
		t.Fatal("失效版本后来变旧时不得因 timeline_id 相等而清除观察要求")
	}
	newerCurrentInspect := schema.ToolMessage(
		`{"status":"succeeded","data":{"timeline_exists":true,`+
			`"timeline_id":"draft_ripple_observation_surface:v3","is_current":true}}`,
		"call_newer_current_inspect",
		schema.WithToolName("timeline.inspect"),
	)
	if timelineObservationRequiredSinceLatestUser(append(messages, newerCurrentInspect)) {
		t.Fatal("成功读取更新的当前版本后仍要求重复观察")
	}
	timelineGoneInspect := schema.ToolMessage(
		`{"status":"succeeded","data":{"timeline_exists":false,"is_current":true}}`,
		"call_timeline_gone_inspect",
		schema.WithToolName("timeline.inspect"),
	)
	if timelineObservationRequiredSinceLatestUser(append(messages, timelineGoneInspect)) {
		t.Fatal("成功确认当前时间线不存在后仍要求重复观察")
	}

	successfulInspect := schema.ToolMessage(
		`{"status":"succeeded","data":{"timeline_exists":true,`+
			`"timeline_id":"draft_ripple_observation_surface:v2",`+
			`"is_current":true}}`, "call_inspect",
		schema.WithToolName("timeline.inspect"),
	)
	afterInspect := append(messages, successfulInspect)
	if timelineObservationRequiredSinceLatestUser(afterInspect) {
		t.Fatal("成功 inspect 后仍要求重复观察")
	}
	specs, err = selectModelToolSurface(ctx, service.tools, afterInspect)
	if err != nil {
		t.Fatal(err)
	}
	if names := surfaceNames(specs); !containsName(names, "timeline.update") {
		t.Fatalf("inspect 后未恢复编辑工具面: %v", names)
	}

	nonRipple := schema.ToolMessage(
		`{"status":"succeeded","data":{"coordinate_effect":{`+
			`"duration_frames_before":60,"duration_frames_after":120,`+
			`"ripple_delta_frames":0,"observation_required":false}}}`,
		"call_append",
		schema.WithToolName("timeline.insert"),
	)
	if timelineObservationRequiredSinceLatestUser([]*schema.Message{user, nonRipple}) {
		t.Fatal("顺序追加主视觉不应强制重复 inspect")
	}
	if timelineObservationRequiredSinceLatestUser(append(messages, schema.UserMessage("只调整字幕"))) {
		t.Fatal("新的真实用户消息必须开始新的观察边界")
	}
}

func TestEveryConfiguredModelSurfaceFitsBudgetAcrossDraftStates(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_all_surface_budgets"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	seedSurfaceTranscript(t, service)
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	surfaces := []rushestools.Surface{
		rushestools.SurfaceDiscovery,
		rushestools.SurfaceTalkingHead,
		rushestools.SurfaceBeatEdit,
		rushestools.SurfaceTimelineEdit,
		rushestools.SurfaceRender,
		rushestools.SurfacePreviewCheck,
		rushestools.SurfaceControl,
	}

	assertBudgets := func(state string) {
		t.Helper()
		allowed, allowedErr := service.tools.Allowed(ctx, true)
		if allowedErr != nil {
			t.Fatal(allowedErr)
		}
		for _, surface := range surfaces {
			specs := filterSurface(allowed, surface)
			if len(specs) == 0 {
				continue
			}
			metrics, metricErr := modelToolSchemaSizeFromTools(ctx, implementationsForSpecs(specs))
			if metricErr != nil {
				t.Fatal(metricErr)
			}
			if len(specs) > maxBoundModelTools || metrics.TotalRunes > maxBoundModelSchemaRunes {
				t.Errorf(
					"%s surface=%d names=%v over budget: tools=%d runes=%d",
					state, surface, surfaceNames(specs), len(specs), metrics.TotalRunes,
				)
			}
		}
	}

	assertBudgets("asset-only")
	setSurfaceTimelineState(t, service, draftID, true)
	seedSurfacePreview(t, service, draftID)
	assertBudgets("timeline-validated-with-preview")
}

func TestRemainingWorkflowSurfaceClassifiesEveryContinuationLane(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want rushestools.Surface
	}{
		{name: "pending_timeline_edit", text: "把第一段剪掉", want: rushestools.SurfaceTimelineEdit},
		{name: "user_final_export", text: "导出最终成片", want: 0},
		{name: "preview_check", text: "检查预览有没有黑帧", want: rushestools.SurfacePreviewCheck},
		{name: "talking_head", text: "读取口播逐字稿", want: rushestools.SurfaceTalkingHead},
		{name: "beat", text: "读取 bgm 的 bpm", want: rushestools.SurfaceBeatEdit},
		{name: "asset_search", text: "为时间线找一个镜头", want: rushestools.SurfaceDiscovery},
		{name: "initial_timeline", text: "组装初版时间线", want: rushestools.SurfaceTimelineEdit},
		{name: "unclassified", text: "你好", want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := remainingWorkflowSurface(test.text); got != test.want {
				t.Fatalf("remainingWorkflowSurface(%q)=%d want=%d", test.text, got, test.want)
			}
		})
	}
	if got := noModelToolsError(rushestools.SurfaceRender).Error(); got !=
		"当前状态没有可绑定的模型工具: surface=16" {
		t.Fatalf("noModelToolsError=%q", got)
	}
}

func TestAtomicTimelineToolsAreTheOnlyEditingSurface(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_no_mixed_edit_surface"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	seedSurfaceTranscript(t, service)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	assertNotMixed := func(prompt string) []string {
		t.Helper()
		specs, selectErr := selectModelToolSurface(
			ctx,
			service.tools,
			[]*schema.Message{schema.UserMessage(prompt)},
		)
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		names := surfaceNames(specs)
		return names
	}

	composeNames := assertNotMixed("请组装初版时间线")
	if !containsName(composeNames, "asset.list_assets") ||
		containsName(composeNames, "timeline.insert") {
		t.Fatalf("initial evidence surface=%v", composeNames)
	}
	firstInsertNames := assertNotMixed("调用 timeline.insert 插入第一个 visual_base clip")
	if !containsName(firstInsertNames, "timeline.insert") {
		t.Fatalf("first insert surface=%v", firstInsertNames)
	}
	setSurfaceTimelineState(t, service, draftID, false)
	talkingHeadNames := assertNotMixed("完成口播气口和重说清理")
	if !containsName(talkingHeadNames, "speech.search") {
		t.Fatalf("talking-head surface=%v", talkingHeadNames)
	}
	beatNames := assertNotMixed("根据节拍和 BGM 做卡点")
	if !containsName(beatNames, "timeline.insert") ||
		!containsName(beatNames, "timeline.update") {
		t.Fatalf("beat surface=%v", beatNames)
	}
	atomicNames := assertNotMixed("只把当前时间线片段音量调低")
	for _, name := range []string{
		"timeline.insert", "timeline.delete", "timeline.update", "timeline.split",
	} {
		if !containsName(atomicNames, name) {
			t.Fatalf("atomic surface=%v missing %s", atomicNames, name)
		}
	}
}

func TestTimelineEditSurfaceCanDiscoverAndInsertNewShot(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_timeline_insert_surface"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	seedSurfaceTranscript(t, service)
	setSurfaceTimelineState(t, service, draftID, false)

	specs, err := selectModelToolSurface(
		rushestools.WithDraftID(t.Context(), draftID),
		service.tools,
		[]*schema.Message{schema.UserMessage("在时间线里找一个海边镜头插入")},
	)
	if err != nil {
		t.Fatal(err)
	}
	names := surfaceNames(specs)
	for _, name := range []string{
		"asset.list_assets",
		"media.detect_shots",
		"shot.search",
	} {
		if !containsName(names, name) {
			t.Errorf("surface=%v missing %s", names, name)
		}
	}
	if containsName(names, "timeline.update") {
		t.Fatalf("检索完成前 surface=%v unexpectedly exposes timeline.update", names)
	}

	specs, err = selectModelToolSurface(
		rushestools.WithDraftID(t.Context(), draftID),
		service.tools,
		[]*schema.Message{
			schema.UserMessage("在时间线里找一个海边镜头插入"),
			schema.ToolMessage(
				`{"query":"海边","shots":[{"shot_id":"shot_surface_1"}],"total_matches":1,"truncated":false}`,
				"call_search_shots",
				schema.WithToolName("shot.search"),
			),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	names = surfaceNames(specs)
	if !containsName(names, "timeline.inspect") ||
		!containsName(names, "timeline.update") ||
		containsName(names, "shot.search") {
		t.Fatalf("检索完成后 surface=%v", names)
	}
}

func TestTimelineEditSurfaceDoesNotAdvanceAfterEmptyOrFailedShotSearch(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_timeline_empty_search_surface"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	seedSurfaceTranscript(t, service)
	setSurfaceTimelineState(t, service, draftID, false)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	for _, result := range []string{
		`{"query":"海边","shots":[],"total_matches":0,"truncated":false}`,
		`{"error_code":"tool_execution_error","observation":"检索失败"}`,
	} {
		specs, selectErr := selectModelToolSurface(ctx, service.tools, []*schema.Message{
			schema.UserMessage("在时间线里找一个海边镜头插入"),
			schema.ToolMessage(result, "call_search_shots", schema.WithToolName("shot.search")),
		})
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		names := surfaceNames(specs)
		if !containsName(names, "shot.search") ||
			containsName(names, "timeline.update") {
			t.Fatalf("search result=%s surface=%v", result, names)
		}
	}
}

func TestSuccessfulShotSearchPreservesSpecializedWorkflowIntent(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_specialized_search_surface"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	seedSurfaceTranscript(t, service)
	setSurfaceTimelineState(t, service, draftID, false)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	result, err := json.Marshal(rushestools.ShotSearchResult{
		Query: "补充镜头",
		Shots: []rushestools.ShotCandidate{{
			ShotID: "shot_surface_1", AssetID: "asset_surface",
			SourceStartFrame: 0, SourceEndFrame: 90, DurationFrames: 90,
		}},
		TotalMatches: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		prompt    string
		required  string
		forbidden []string
	}{
		{
			name: "talking_head", prompt: "清理口播并找一个 B-roll 镜头插入",
			required:  "speech.search",
			forbidden: []string{"timeline.update"},
		},
		{
			name: "beat_edit", prompt: "按 BGM 卡点并找一个镜头插入",
			required: "timeline.insert",
		},
		{
			name: "generic_timeline_edit", prompt: "在时间线里找一个海边镜头插入",
			required: "timeline.update",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			specs, selectErr := selectModelToolSurface(ctx, service.tools, []*schema.Message{
				schema.UserMessage(test.prompt),
				schema.ToolMessage(
					string(result),
					"call_search_shots",
					schema.WithToolName("shot.search"),
				),
			})
			if selectErr != nil {
				t.Fatal(selectErr)
			}
			names := surfaceNames(specs)
			if !containsName(names, test.required) {
				t.Fatalf("surface=%v missing %s", names, test.required)
			}
			for _, forbidden := range test.forbidden {
				if containsName(names, forbidden) {
					t.Fatalf("surface=%v unexpectedly contains %s", names, forbidden)
				}
			}
		})
	}
}

func TestDecisionContinuationPreservesOriginalEditIntent(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_decision_continuation_surface"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	setSurfaceTimelineState(t, service, draftID, true)
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	specs, err := selectModelToolSurface(ctx, service.tools, []*schema.Message{
		schema.UserMessage("剪掉开头三秒，渲染预览并检查黑帧"),
		schema.UserMessage(
			"用户刚刚回答了你此前提出的选择题。\n问题：保留开头吗？\n回答：删除\n" +
				"这是同一条任务的继续，不是新的请求。请继续。",
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	names := surfaceNames(specs)
	if !containsName(names, "timeline.update") || containsName(names, "preview.check") {
		t.Fatalf("真实用户回答后未保留原始编辑意图 surface=%v", names)
	}
}

func TestUnclassifiedEditLanguageConservativelyKeepsTimelineTools(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_unclassified_edit_surface"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	setSurfaceTimelineState(t, service, draftID, true)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	for _, prompt := range []string{"把前面三秒去掉", "把第一段缩短一点"} {
		specs, selectErr := selectModelToolSurface(ctx, service.tools, []*schema.Message{
			schema.UserMessage(prompt),
		})
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		names := surfaceNames(specs)
		if !containsName(names, "timeline.update") ||
			!containsName(names, "timeline.check") {
			t.Errorf("%q surface=%v", prompt, names)
		}
	}
}

func TestDynamicModelToolSurfacePreservesReadOnlyTimelineInspect(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_read_only_surface"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	for _, prompt := range []string{"读取当前时间线", "调用 timeline.inspect 查看状态"} {
		specs, selectErr := selectModelToolSurface(ctx, service.tools, []*schema.Message{
			schema.UserMessage(prompt),
		})
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		if names := surfaceNames(specs); !reflect.DeepEqual(names, []string{"timeline.inspect"}) {
			t.Errorf("%q surface=%v want=[timeline.inspect]", prompt, names)
		}
	}
}

func TestDynamicModelToolSurfaceFallsBackToPrerequisiteStage(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_surface_prerequisite"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	editSpecs, err := selectModelToolSurface(ctx, service.tools, []*schema.Message{
		schema.UserMessage("帮我剪辑这些素材"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if names := surfaceNames(editSpecs); !containsName(names, "timeline.insert") {
		t.Fatalf("明确剪辑意图未进入 mutation stage: %v", names)
	}
	renderSpecs, err := selectModelToolSurface(ctx, service.tools, []*schema.Message{
		schema.UserMessage("渲染预览并做黑帧质检"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if names := surfaceNames(renderSpecs); !containsName(names, "asset.list_assets") ||
		containsName(names, "timeline.insert") || containsName(names, "timeline.inspect") {
		t.Fatalf("缺少时间线时 render surface=%v want discovery evidence tools", names)
	}

	for _, prompt := range []string{
		"导出最终成片并下载 MP4",
		"渲染最终视频",
		"渲染最终成片",
		"渲染成片",
	} {
		exportSpecs, selectErr := selectModelToolSurface(ctx, service.tools, []*schema.Message{
			schema.UserMessage(prompt),
		})
		if selectErr != nil {
			t.Fatalf("%q select surface: %v", prompt, selectErr)
		}
		if names := surfaceNames(exportSpecs); !reflect.DeepEqual(names, []string{"timeline.inspect"}) {
			t.Fatalf("%q 是用户最终导出，不得暴露编辑或预览能力: %v", prompt, names)
		}
	}

	setSurfaceTimelineState(t, service, draftID, true)
	specs, err := selectModelToolSurface(ctx, service.tools, []*schema.Message{
		schema.UserMessage("质检预览是否有黑帧和静音"),
	})
	if err != nil {
		t.Fatal(err)
	}
	names := surfaceNames(specs)
	if !containsName(names, "preview.generate") || containsName(names, "preview.check") {
		t.Fatalf("缺少预览时 surface=%v want render prerequisite tools", names)
	}
}

func TestSpecializedSurfaceFallsBackUntilAssetsAreUsable(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_specialized_surface_prerequisite"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedUnavailableSurfaceAsset(t, service, draftID)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	for _, prompt := range []string{
		"完成口播气口和重说清理",
		"根据节拍和 BGM 做卡点",
	} {
		specs, selectErr := selectModelToolSurface(ctx, service.tools, []*schema.Message{
			schema.UserMessage(prompt),
		})
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		names := surfaceNames(specs)
		if !containsName(names, "asset.list_assets") || containsName(names, "media.detect_shots") {
			t.Errorf("%q surface=%v want discovery prerequisite tools", prompt, names)
		}
	}
}

func TestExplicitIntentAdvancesAfterSuccessfulWorkflowWrite(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_explicit_surface_advance"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	seedSurfaceTranscript(t, service)
	setSurfaceTimelineState(t, service, draftID, false)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	tests := []struct {
		name      string
		prompt    string
		toolName  string
		required  string
		forbidden string
	}{
		{
			name: "initial_insert", prompt: "请组装初版时间线",
			toolName: "timeline.insert",
			required: "timeline.update",
		},
		{
			name: "talking_head_after_initial_insert", prompt: "完成口播气口和重说清理",
			toolName: "timeline.insert",
			required: "speech.search",
		},
		{
			name: "talking_head_evidence", prompt: "完成口播气口和重说清理",
			toolName: "speech.search",
			required: "timeline.delete",
		},
		{
			name: "beat", prompt: "根据节拍和 BGM 做卡点",
			toolName: "timeline.insert",
			required: "timeline.check",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			specs, selectErr := selectModelToolSurface(ctx, service.tools, []*schema.Message{
				schema.UserMessage(test.prompt),
				schema.ToolMessage(
					`{"status":"succeeded"}`,
					"call_success",
					schema.WithToolName(test.toolName),
				),
			})
			if selectErr != nil {
				t.Fatal(selectErr)
			}
			names := surfaceNames(specs)
			if !containsName(names, test.required) || containsName(names, test.forbidden) {
				t.Fatalf("surface=%v want %s without %s", names, test.required, test.forbidden)
			}
		})
	}
}

func TestStagedEditThenRenderRequestStartsWithEditSurface(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_composite_edit_render_surface"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	seedSurfaceTranscript(t, service)
	setSurfaceTimelineState(t, service, draftID, false)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	tests := []struct {
		prompt   string
		editTool string
	}{
		{"剪掉开头三秒然后导出 MP4", "timeline.update"},
		{"按 BGM 卡点后渲染预览", "timeline.insert"},
	}
	for _, test := range tests {
		t.Run(test.editTool, func(t *testing.T) {
			messages := []*schema.Message{schema.UserMessage(test.prompt)}
			specs, selectErr := selectModelToolSurface(ctx, service.tools, messages)
			if selectErr != nil {
				t.Fatal(selectErr)
			}
			names := surfaceNames(specs)
			if !containsName(names, test.editTool) {
				t.Fatalf("初始 surface=%v", names)
			}

			specs, selectErr = selectModelToolSurface(ctx, service.tools, append(messages,
				schema.ToolMessage(
					`{"status":"succeeded"}`,
					"call_edit_success",
					schema.WithToolName(test.editTool),
				),
			))
			if selectErr != nil {
				t.Fatal(selectErr)
			}
			names = surfaceNames(specs)
			if !containsName(names, "timeline.check") ||
				containsName(names, "preview.generate") {
				t.Fatalf("编辑完成后 surface=%v", names)
			}
		})
	}

	talkingMessages := []*schema.Message{schema.UserMessage("清理口播气口和重说后导出")}
	specs, err := selectModelToolSurface(ctx, service.tools, talkingMessages)
	if err != nil {
		t.Fatal(err)
	}
	if names := surfaceNames(specs); !containsName(names, "speech.search") {
		t.Fatalf("口播证据阶段 surface=%v", names)
	}
	talkingMessages = append(talkingMessages, schema.ToolMessage(
		`{"status":"succeeded","transcript_id":"transcript_1","utterances":[]}`,
		"call_speech_search",
		schema.WithToolName("speech.search"),
	))
	specs, err = selectModelToolSurface(ctx, service.tools, talkingMessages)
	if err != nil {
		t.Fatal(err)
	}
	if names := surfaceNames(specs); !containsName(names, "timeline.delete") ||
		!containsName(names, "speech.search") ||
		!containsName(names, "shot.search") {
		t.Fatalf("口播原子编辑阶段 surface=%v", names)
	}
	talkingMessages = append(talkingMessages, schema.ToolMessage(
		`{"status":"succeeded","timeline_version":2}`,
		"call_delete",
		schema.WithToolName("timeline.delete"),
	))
	specs, err = selectModelToolSurface(ctx, service.tools, talkingMessages)
	if err != nil {
		t.Fatal(err)
	}
	if names := surfaceNames(specs); !containsName(names, "timeline.inspect") ||
		!containsName(names, "speech.search") ||
		!containsName(names, "shot.search") {
		t.Fatalf("口播删剪后的重新观察 surface=%v", names)
	}
}

func TestRequestsShotSearchRecognizesNaturalRetrievalWording(t *testing.T) {
	t.Parallel()
	for _, prompt := range []string{
		"请检索镜头，找海边日落",
		"做一次镜头检索，不要剪辑",
		"只检索当前草稿里适合火焰段落的 B-roll",
		"从已理解素材中检索适合收尾的片段",
	} {
		if !requestsShotSearch(strings.ToLower(prompt)) {
			t.Errorf("未识别镜头检索表达: %q", prompt)
		}
	}
	for _, prompt := range []string{"检索口播台词", "查看时间线", "分析 BGM 拍点"} {
		if requestsShotSearch(strings.ToLower(prompt)) {
			t.Errorf("误识别为镜头检索: %q", prompt)
		}
	}
}

func TestStagedEditRenderAndPreviewCheckPreservesStageOrder(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_composite_edit_preview_check"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	seedSurfaceTranscript(t, service)
	setSurfaceTimelineState(t, service, draftID, false)
	if _, err := service.database.Write().ExecContext(t.Context(), `
		INSERT INTO objects(hash,rel_path,size,created_at)
		VALUES('composite_preview_hash','composite_preview.mp4',1,'2026-01-01T00:00:00Z');
		INSERT INTO previews(
			preview_id,draft_id,timeline_version,object_hash,quality_json,created_at
		) VALUES(
			'composite_preview',?,1,'composite_preview_hash','{}','2026-01-01T00:00:00Z'
		)`, draftID); err != nil {
		t.Fatal(err)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	for _, prompt := range []string{
		"剪掉开头三秒，渲染预览并检查黑帧",
		"先调用 timeline.update，再调用 preview.check",
	} {
		specs, selectErr := selectModelToolSurface(ctx, service.tools, []*schema.Message{
			schema.UserMessage(prompt),
		})
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		names := surfaceNames(specs)
		if !containsName(names, "timeline.update") ||
			containsName(names, "preview.check") {
			t.Errorf("%q initial surface=%v", prompt, names)
		}
	}
}

func TestWorkflowTransitionIgnoresUnrelatedFailureButLatestSameToolWins(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_transition_failure_order"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	setSurfaceTimelineState(t, service, draftID, false)
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	user := schema.UserMessage("剪掉开头三秒然后导出")
	success := schema.ToolMessage(
		`{"status":"succeeded"}`,
		"call_apply_success",
		schema.WithToolName("timeline.update"),
	)
	planFailure := schema.ToolMessage(
		`{"status":"failed","error_code":"invalid_arguments"}`,
		"call_plan_failure",
		schema.WithToolName("plan.update"),
	)

	for _, messages := range [][]*schema.Message{
		{user, success, planFailure},
		{user, planFailure, success},
	} {
		specs, selectErr := selectModelToolSurface(ctx, service.tools, messages)
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		names := surfaceNames(specs)
		if !containsName(names, "timeline.check") ||
			!containsName(names, "timeline.update") {
			t.Fatalf("unrelated failure messages=%v surface=%v", messages, names)
		}
	}

	specs, err := selectModelToolSurface(ctx, service.tools, []*schema.Message{
		user,
		success,
		schema.ToolMessage(
			`{"status":"failed","error_code":"invalid_arguments"}`,
			"call_apply_failure",
			schema.WithToolName("timeline.update"),
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	names := surfaceNames(specs)
	if !containsName(names, "timeline.update") {
		t.Fatalf("newer same-tool failure surface=%v", names)
	}
}

func TestGenericEditSurfaceRemainsAvailableUntilExplicitValidation(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_multistep_generic_edit"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	setSurfaceTimelineState(t, service, draftID, false)
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	messages := []*schema.Message{
		schema.UserMessage("插入素材，读取生成的 clip ID，再调低该片段音量并渲染预览"),
		schema.ToolMessage(
			`{"status":"succeeded"}`,
			"call_insert",
			schema.WithToolName("timeline.update"),
		),
	}

	specs, err := selectModelToolSurface(ctx, service.tools, messages)
	if err != nil {
		t.Fatal(err)
	}
	names := surfaceNames(specs)
	if !containsName(names, "timeline.update") ||
		!containsName(names, "timeline.inspect") ||
		!containsName(names, "timeline.check") ||
		containsName(names, "preview.generate") {
		t.Fatalf("首次 patch 后 surface=%v", names)
	}

	setSurfaceTimelineState(t, service, draftID, true)
	specs, err = selectModelToolSurface(ctx, service.tools, append(messages,
		schema.ToolMessage(
			`{"status":"succeeded"}`,
			"call_validate",
			schema.WithToolName("timeline.check"),
		),
	))
	if err != nil {
		t.Fatal(err)
	}
	names = surfaceNames(specs)
	if !containsName(names, "preview.generate") ||
		containsName(names, "timeline.update") {
		t.Fatalf("显式验证后 surface=%v", names)
	}
}

func TestSuccessfulPreviewJobAdvancesWorkflowToInspection(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_preview_success_transition"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	setSurfaceTimelineState(t, service, draftID, true)
	if _, err := service.database.Write().ExecContext(t.Context(), `
		INSERT INTO objects(hash,rel_path,size,created_at)
		VALUES('transition_preview_hash','transition_preview.mp4',1,'2026-01-01T00:00:00Z');
		INSERT INTO previews(
			preview_id,draft_id,timeline_version,object_hash,quality_json,created_at
		) VALUES(
			'transition_preview',?,1,'transition_preview_hash','{}','2026-01-01T00:00:00Z'
		)`, draftID); err != nil {
		t.Fatal(err)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	prompt := "剪掉开头三秒，渲染预览并检查黑帧"
	base := []*schema.Message{
		schema.UserMessage(prompt),
		schema.ToolMessage(
			`{"status":"succeeded"}`,
			"call_apply",
			schema.WithToolName("timeline.update"),
		),
		schema.ToolMessage(
			`{"status":"succeeded"}`,
			"call_validate",
			schema.WithToolName("timeline.check"),
		),
	}

	specs, err := selectModelToolSurface(ctx, service.tools, append(base,
		schema.ToolMessage(
			`{"status":"failed","error_code":"render_failed"}`,
			"call_preview_failed",
			schema.WithToolName("preview.generate"),
		),
	))
	if err != nil {
		t.Fatal(err)
	}
	if names := surfaceNames(specs); !containsName(names, "preview.generate") ||
		containsName(names, "preview.check") {
		t.Fatalf("preview failure surface=%v", names)
	}

	specs, err = selectModelToolSurface(ctx, service.tools, append(base,
		schema.ToolMessage(
			`{"status":"timeout","data":{"job_id":"preview_job","job_status":"running"}}`,
			"call_preview_timeout",
			schema.WithToolName("preview.generate"),
		),
	))
	if err != nil {
		t.Fatal(err)
	}
	if names := surfaceNames(specs); !containsName(names, "preview.generate") ||
		containsName(names, "preview.check") {
		t.Fatalf("preview timeout surface=%v", names)
	}

	specs, err = selectModelToolSurface(ctx, service.tools, append(base,
		schema.ToolMessage(
			`{"status":"succeeded","data":{"preview_id":"transition_preview","job_id":"preview_job","job_status":"succeeded","timeline_id":"draft_preview_success_transition:v1","timeline_version":1,"orientation":"auto"}}`,
			"call_preview_success",
			schema.WithToolName("preview.generate"),
		),
	))
	if err != nil {
		t.Fatal(err)
	}
	if names := surfaceNames(specs); !containsName(names, "preview.check") ||
		containsName(names, "preview.generate") {
		t.Fatalf("preview completed surface=%v", names)
	}
}

func TestRenderAndInspectIgnoresPreviewFromOlderTimeline(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_preview_stale_transition"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	setSurfaceTimelineState(t, service, draftID, true)
	seedSurfacePreview(t, service, draftID)

	specs, err := selectModelToolSurface(
		rushestools.WithDraftID(t.Context(), draftID),
		service.tools,
		[]*schema.Message{
			schema.UserMessage("当前时间线已更新，请渲染新预览并检查黑帧"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	names := surfaceNames(specs)
	if !containsName(names, "preview.generate") || containsName(names, "preview.check") {
		t.Fatalf("旧 preview 不得跳过当前目标渲染，surface=%v", names)
	}
}

func TestExplicitStagedEditThenRenderStartsWithEditOnValidatedTimeline(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_explicit_composite_edit_render"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	setSurfaceTimelineState(t, service, draftID, true)

	specs, err := selectModelToolSurface(
		rushestools.WithDraftID(t.Context(), draftID),
		service.tools,
		[]*schema.Message{
			schema.UserMessage("先调用 timeline.update，再生成 preview"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	names := surfaceNames(specs)
	if !containsName(names, "timeline.update") ||
		containsName(names, "preview.generate") {
		t.Fatalf("已验证时间线的精确复合请求 surface=%v", names)
	}
}

func TestLatestWorkflowFailureDoesNotReuseOlderSuccess(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_latest_workflow_failure"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	setSurfaceTimelineState(t, service, draftID, false)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	t.Run("generic_edit_retry_failed", func(t *testing.T) {
		specs, selectErr := selectModelToolSurface(ctx, service.tools, []*schema.Message{
			schema.UserMessage("剪掉开头三秒然后导出"),
			schema.ToolMessage(
				`{"status":"succeeded"}`,
				"call_apply_success",
				schema.WithToolName("timeline.update"),
			),
			schema.ToolMessage(
				`{"status":"failed","observation":"补丁重试失败"}`,
				"call_apply_failed",
				schema.WithToolName("timeline.update"),
			),
		})
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		names := surfaceNames(specs)
		if !containsName(names, "timeline.update") {
			t.Fatalf("最新编辑失败后 surface=%v", names)
		}
	})

	t.Run("shot_search_retry_empty", func(t *testing.T) {
		nonEmpty, marshalErr := json.Marshal(rushestools.ShotSearchResult{
			Query: "海边",
			Shots: []rushestools.ShotCandidate{{
				ShotID: "shot_surface_old", AssetID: "asset_surface",
				SourceStartFrame: 0, SourceEndFrame: 90, DurationFrames: 90,
			}},
			TotalMatches: 1,
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		empty, marshalErr := json.Marshal(rushestools.ShotSearchResult{
			Query: "海边",
			Shots: []rushestools.ShotCandidate{},
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		specs, selectErr := selectModelToolSurface(ctx, service.tools, []*schema.Message{
			schema.UserMessage("在时间线里找一个海边镜头插入"),
			schema.ToolMessage(
				string(nonEmpty),
				"call_search_non_empty",
				schema.WithToolName("shot.search"),
			),
			schema.ToolMessage(
				string(empty),
				"call_search_empty",
				schema.WithToolName("shot.search"),
			),
		})
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		names := surfaceNames(specs)
		if !containsName(names, "shot.search") ||
			containsName(names, "timeline.update") {
			t.Fatalf("最新镜头检索为空后 surface=%v", names)
		}
	})
}

func TestMultipleAtomicEditsFinishBeforeRender(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_composite_specialized_generic_edit"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	setSurfaceTimelineState(t, service, draftID, false)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	messages := []*schema.Message{
		schema.UserMessage("清理口播并添加字幕后导出"),
		schema.ToolMessage(
			`{"status":"succeeded","transcript_id":"transcript_1","utterances":[]}`,
			"call_speech_search",
			schema.WithToolName("speech.search"),
		),
	}
	specs, err := selectModelToolSurface(ctx, service.tools, messages)
	if err != nil {
		t.Fatal(err)
	}
	names := surfaceNames(specs)
	if !containsName(names, "timeline.update") {
		t.Fatalf("口播证据读取后 surface=%v", names)
	}

	specs, err = selectModelToolSurface(ctx, service.tools, append(messages,
		schema.ToolMessage(
			`{"status":"succeeded"}`,
			"call_timeline_update",
			schema.WithToolName("timeline.update"),
		),
	))
	if err != nil {
		t.Fatal(err)
	}
	names = surfaceNames(specs)
	if !containsName(names, "timeline.check") ||
		!containsName(names, "timeline.update") {
		t.Fatalf("全部编辑完成后 surface=%v", names)
	}

	setSurfaceTimelineState(t, service, draftID, true)
	specs, err = selectModelToolSurface(ctx, service.tools, append(messages,
		schema.ToolMessage(
			`{"status":"succeeded"}`,
			"call_timeline_update",
			schema.WithToolName("timeline.update"),
		),
		schema.ToolMessage(
			`{"status":"succeeded"}`,
			"call_validate",
			schema.WithToolName("timeline.check"),
		),
	))
	if err != nil {
		t.Fatal(err)
	}
	names = surfaceNames(specs)
	if !containsName(names, "preview.generate") || containsName(names, "timeline.update") {
		t.Fatalf("验证完成后 surface=%v", names)
	}
}

func TestPlanUpdateIsAvailableAcrossWorkflowSurfaces(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_plan_workflow_surface"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	for _, prompt := range []string{
		"请组装初版时间线",
		"读取口播台词和气口证据",
		"根据节拍和 BGM 做卡点",
	} {
		specs, selectErr := selectModelToolSurface(ctx, service.tools, []*schema.Message{
			schema.UserMessage(prompt),
		})
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		if names := surfaceNames(specs); !containsName(names, "plan.update") {
			t.Errorf("%q surface=%v missing plan.update", prompt, names)
		}
	}

	setSurfaceTimelineState(t, service, draftID, false)
	for _, prompt := range []string{"验证时间线后渲染预览"} {
		specs, selectErr := selectModelToolSurface(ctx, service.tools, []*schema.Message{
			schema.UserMessage(prompt),
		})
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		if names := surfaceNames(specs); !containsName(names, "plan.update") {
			t.Errorf("%q surface=%v missing plan.update", prompt, names)
		}
	}

	budgetMessages := []*schema.Message{
		schema.SystemMessage(coreSystemPrompt +
			"\n\n【工具预算提醒】本回合剩余 3 次模型与工具往返。请立即开始收敛：先用 plan.update 固化已确定但未执行的计划要点。"),
		schema.UserMessage("只修改当前时间线片段音量"),
	}
	specs, err := selectModelToolSurface(ctx, service.tools, budgetMessages)
	if err != nil {
		t.Fatal(err)
	}
	names := surfaceNames(specs)
	if !containsName(names, "plan.update") || containsName(names, "timeline.update") {
		t.Fatalf("预算提醒 surface=%v", names)
	}

	specs, err = selectModelToolSurface(ctx, service.tools, append(budgetMessages,
		schema.ToolMessage(
			`{"status":"succeeded"}`,
			"call_plan_update",
			schema.WithToolName("plan.update"),
		),
	))
	if err != nil {
		t.Fatal(err)
	}
	names = surfaceNames(specs)
	if !containsName(names, "timeline.update") ||
		containsName(names, "memory.set") ||
		containsName(names, "memory.remove") {
		t.Fatalf("计划固化后 surface=%v", names)
	}

	specs, err = selectModelToolSurface(ctx, service.tools, append(budgetMessages,
		schema.ToolMessage(
			`{"status":"succeeded"}`,
			"call_plan_update_success",
			schema.WithToolName("plan.update"),
		),
		schema.ToolMessage(
			`{"status":"failed","error_code":"invalid_arguments"}`,
			"call_plan_update_failure",
			schema.WithToolName("plan.update"),
		),
	))
	if err != nil {
		t.Fatal(err)
	}
	names = surfaceNames(specs)
	if !containsName(names, "plan.update") || containsName(names, "timeline.update") {
		t.Fatalf("最新计划失败 surface=%v", names)
	}
}

func TestSuccessfulControlActionAdvancesStagedRequest(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_control_composite"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	setSurfaceTimelineState(t, service, draftID, false)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	for _, test := range []struct {
		prompt   string
		toolName string
		wantTool string
	}{
		{"更新计划，然后剪掉开头三秒并导出", "plan.update", "timeline.update"},
		{"记住我偏好短片，然后剪掉开头三秒", "memory.set", "timeline.update"},
	} {
		messages := []*schema.Message{
			schema.UserMessage(test.prompt),
			schema.ToolMessage(
				`{"status":"succeeded"}`,
				"call_control_success",
				schema.WithToolName(test.toolName),
			),
		}
		specs, selectErr := selectModelToolSurface(ctx, service.tools, messages)
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		names := surfaceNames(specs)
		if !containsName(names, test.wantTool) ||
			containsName(names, "memory.set") ||
			containsName(names, "memory.remove") {
			t.Errorf("%q after %s surface=%v", test.prompt, test.toolName, names)
		}
	}
}

func TestPreviewCheckIntentTakesPriorityOverMediaNouns(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_preview_check_nouns"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	setSurfaceTimelineState(t, service, draftID, true)
	seedSurfacePreview(t, service, draftID)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	for _, prompt := range []string{
		"质检预览音频是否静音",
		"质检字幕是否正常",
	} {
		specs, selectErr := selectModelToolSurface(ctx, service.tools, []*schema.Message{
			schema.UserMessage(prompt),
		})
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		names := surfaceNames(specs)
		if !containsName(names, "preview.check") ||
			containsName(names, "audio.analyze_beats") ||
			containsName(names, "timeline.update") {
			t.Errorf("%q surface=%v", prompt, names)
		}
	}
}

func TestBroadRequestAdvancesSurfaceWhenDraftStateChanges(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_surface_state_advance"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	messages := []*schema.Message{schema.UserMessage("用这些素材做一个完整短片")}

	before, err := selectModelToolSurface(ctx, service.tools, messages)
	if err != nil {
		t.Fatal(err)
	}
	beforeNames := surfaceNames(before)
	if !containsName(beforeNames, "asset.list_assets") ||
		containsName(beforeNames, "timeline.insert") ||
		containsName(beforeNames, "timeline.update") {
		t.Fatalf("初始 surface=%v", beforeNames)
	}
	messages = append(messages, schema.ToolMessage(
		`{"draft_id":"draft_surface_state_advance","assets":[{}]}`,
		"call_list_assets",
		schema.WithToolName("asset.list_assets"),
	), schema.ToolMessage(
		`{"shots":[{"shot_id":"shot-a"}]}`,
		"call_shot_search",
		schema.WithToolName("shot.search"),
	))
	afterEvidence, err := selectModelToolSurface(ctx, service.tools, messages)
	if err != nil {
		t.Fatal(err)
	}
	if names := surfaceNames(afterEvidence); !containsName(names, "timeline.insert") {
		t.Fatalf("素材证据完成后 surface=%v", names)
	}

	setSurfaceTimelineState(t, service, draftID, true)
	afterComposeMessages := append(messages,
		schema.ToolMessage(
			`{"status":"succeeded","observation":"首个片段已插入"}`,
			"call_insert",
			schema.WithToolName("timeline.insert"),
		),
	)
	after, err := selectModelToolSurface(ctx, service.tools, afterComposeMessages)
	if err != nil {
		t.Fatal(err)
	}
	afterNames := surfaceNames(after)
	if !containsName(afterNames, "timeline.update") {
		t.Fatalf("状态推进后 surface=%v", afterNames)
	}

	setSurfaceTimelineState(t, service, draftID, false)
	afterEditMessages := append(messages,
		schema.ToolMessage(
			`{"status":"succeeded","observation":"时间线补丁已应用"}`,
			"call_apply",
			schema.WithToolName("timeline.update"),
		),
	)
	afterEdit, err := selectModelToolSurface(ctx, service.tools, afterEditMessages)
	if err != nil {
		t.Fatal(err)
	}
	afterEditNames := surfaceNames(afterEdit)
	if !containsName(afterEditNames, "timeline.check") ||
		!containsName(afterEditNames, "timeline.update") {
		t.Fatalf("编辑完成后 surface=%v", afterEditNames)
	}

	setSurfaceTimelineState(t, service, draftID, true)
	afterValidation, err := selectModelToolSurface(ctx, service.tools, append(afterEditMessages,
		schema.ToolMessage(
			`{"status":"succeeded"}`,
			"call_validate",
			schema.WithToolName("timeline.check"),
		),
	))
	if err != nil {
		t.Fatal(err)
	}
	afterValidationNames := surfaceNames(afterValidation)
	if !containsName(afterValidationNames, "preview.generate") ||
		containsName(afterValidationNames, "timeline.update") {
		t.Fatalf("验证完成后 surface=%v", afterValidationNames)
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

type surfaceHistoryModel struct {
	mu      sync.Mutex
	history [][]string
}

type finalExportProviderSpy struct {
	mu         sync.Mutex
	boundTools []string
	calls      int
}

func (stub *finalExportProviderSpy) WithTools(
	infos []*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Name)
	}
	stub.mu.Lock()
	stub.boundTools = append([]string(nil), names...)
	stub.mu.Unlock()
	return stub, nil
}

func (stub *finalExportProviderSpy) Generate(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.Message, error) {
	stub.mu.Lock()
	stub.calls++
	stub.mu.Unlock()
	return schema.AssistantMessage(
		"编辑已完成，请在编辑器右侧选择规格并点击导出；完成后可直接下载。",
		nil,
	), nil
}

func (stub *finalExportProviderSpy) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	response, err := stub.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{response}), nil
}

func (stub *finalExportProviderSpy) snapshot() ([]string, int) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return append([]string(nil), stub.boundTools...), stub.calls
}

func (stub *surfaceHistoryModel) WithTools(
	infos []*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Name)
	}
	stub.mu.Lock()
	stub.history = append(stub.history, names)
	stub.mu.Unlock()
	return stub, nil
}

func (*surfaceHistoryModel) Generate(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.Message, error) {
	return schema.AssistantMessage("已完成", nil), nil
}

func (stub *surfaceHistoryModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	response, err := stub.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{response}), nil
}

func (stub *surfaceHistoryModel) snapshots() [][]string {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	result := make([][]string, len(stub.history))
	for index := range stub.history {
		result[index] = append([]string(nil), stub.history[index]...)
	}
	return result
}

func TestPureTimelineCheckBindsOnlyCheckWithoutEditLease(t *testing.T) {
	for _, prompt := range []string{
		"请校验当前时间线不变量和节拍对齐数据。",
		"请调用 timeline.check 检查当前时间线。",
		"请校验当前时间线，不要编辑或渲染。",
		"请校验当前时间线，不编辑不渲染。",
		"请校验当前时间线，请别直接编辑或渲染。",
		"严格检查稳定版本 draft-pure-timeline-check-no-lease:v1 的结构、内容合同和卡点比例，并如实返回是否通过；不要编辑或渲染。",
	} {
		t.Run(prompt, func(t *testing.T) {
			database := agenttest.AgentTestDatabase(t)
			const draftID = "draft-pure-timeline-check-no-lease"
			agenttest.CreateAgentDraft(t, database, draftID)
			service, err := NewService(t.Context(), database, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(service.Close)
			if _, err := seedTimelineVersion(
				service,
				rushestools.WithTimelineMutationOrigin(t.Context(), "manual"),
				draftID,
				timeline.Empty(draftID, 1),
				"pure_timeline_check_fixture",
				nil,
			); err != nil {
				t.Fatal(err)
			}

			ctx := withTestTurnLeaseSession(t, service, t.Context(), draftID)
			spy := &editLeaseProviderSpy{
				database: database, draftID: draftID, expectedLive: []bool{false},
			}
			surface := &dynamicToolSurfaceModel{inner: spy, registry: service.tools}
			if _, err := surface.Generate(ctx, []*schema.Message{schema.UserMessage(prompt)}); err != nil {
				t.Fatal(err)
			}
			if spy.calls != 1 || len(spy.bound) != 1 ||
				!reflect.DeepEqual(spy.bound[0], []string{"timeline.check"}) {
				t.Fatalf("provider calls=%d bound=%v", spy.calls, spy.bound)
			}
			if session := timelineEditLeaseSessionFromContext(ctx); session.activeTurnID() != "" {
				t.Fatalf("纯时间线校验提前取得 edit lease: %q", session.activeTurnID())
			}
			var leases int
			if err := database.Read().QueryRowContext(t.Context(),
				"SELECT COUNT(*) FROM agent_edit_leases WHERE draft_id=?", draftID,
			).Scan(&leases); err != nil || leases != 0 {
				t.Fatalf("纯时间线校验结束后 edit_leases=%d err=%v", leases, err)
			}
		})
	}
}

func TestTimelineCheckWithPositivePreviewKeepsPreviewAndLease(t *testing.T) {
	for _, prompt := range []string{
		"无需编辑时间线只需渲染预览并校验当前时间线",
		"不要修改而是生成预览并校验时间线",
		"不要修改直接生成预览并校验时间线",
	} {
		t.Run(prompt, func(t *testing.T) {
			database := agenttest.AgentTestDatabase(t)
			const draftID = "draft-check-with-positive-preview"
			agenttest.CreateAgentDraft(t, database, draftID)
			service, err := NewService(t.Context(), database, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(service.Close)
			if _, err := seedTimelineVersion(
				service,
				rushestools.WithTimelineMutationOrigin(t.Context(), "manual"),
				draftID,
				timeline.Empty(draftID, 1),
				"check_with_positive_preview_fixture",
				nil,
			); err != nil {
				t.Fatal(err)
			}

			ctx := withTestTurnLeaseSession(t, service, t.Context(), draftID)
			spy := &editLeaseProviderSpy{
				database: database, draftID: draftID, expectedLive: []bool{true},
			}
			surface := &dynamicToolSurfaceModel{inner: spy, registry: service.tools}
			if _, err := surface.Generate(ctx, []*schema.Message{schema.UserMessage(prompt)}); err != nil {
				t.Fatal(err)
			}
			if spy.calls != 1 || len(spy.bound) != 1 ||
				!containsName(spy.bound[0], "timeline.check") ||
				!containsName(spy.bound[0], "preview.generate") {
				t.Fatalf("正向预览复合请求 provider calls=%d bound=%v", spy.calls, spy.bound)
			}
			if session := timelineEditLeaseSessionFromContext(ctx); session.activeTurnID() == "" {
				t.Fatal("正向预览复合请求未取得 edit lease")
			}
		})
	}
}

func TestEditThenTimelineCheckKeepsSameTurnLease(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft-edit-then-check-same-lease"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if _, err := seedTimelineVersion(
		service,
		rushestools.WithTimelineMutationOrigin(t.Context(), "manual"),
		draftID,
		timeline.Empty(draftID, 1),
		"edit_then_check_fixture",
		nil,
	); err != nil {
		t.Fatal(err)
	}

	ctx := withTestTurnLeaseSession(t, service, t.Context(), draftID)
	spy := &editLeaseProviderSpy{
		database: database, draftID: draftID, expectedLive: []bool{true, true},
	}
	surface := &dynamicToolSurfaceModel{inner: spy, registry: service.tools}
	user := schema.UserMessage("把时间线片段音量调低后校验当前时间线")
	if _, err := surface.Generate(ctx, []*schema.Message{user}); err != nil {
		t.Fatal(err)
	}
	if !containsName(spy.bound[0], "timeline.update") ||
		!containsName(spy.bound[0], "timeline.check") {
		t.Fatalf("组合编辑首轮工具面=%v", spy.bound[0])
	}
	var firstTurnID, firstToken string
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT turn_id,lease_token FROM agent_edit_leases WHERE draft_id=?`, draftID,
	).Scan(&firstTurnID, &firstToken); err != nil {
		t.Fatal(err)
	}

	if _, err := surface.Generate(ctx, []*schema.Message{
		user,
		schema.ToolMessage(
			`{"status":"succeeded","data":{"timeline_id":"draft-edit-then-check-same-lease:v2"}}`,
			"call-timeline-update",
			schema.WithToolName("timeline.update"),
		),
	}); err != nil {
		t.Fatal(err)
	}
	if spy.calls != 2 || !containsName(spy.bound[1], "timeline.check") {
		t.Fatalf("编辑后校验工具面=%v calls=%d", spy.bound[1], spy.calls)
	}
	var secondTurnID, secondToken string
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT turn_id,lease_token FROM agent_edit_leases WHERE draft_id=?`, draftID,
	).Scan(&secondTurnID, &secondToken); err != nil {
		t.Fatal(err)
	}
	if firstTurnID == "" || firstToken == "" ||
		firstTurnID != secondTurnID || firstToken != secondToken {
		t.Fatalf(
			"编辑后校验未保持同一 lease: first=%s/%s second=%s/%s",
			firstTurnID, firstToken, secondTurnID, secondToken,
		)
	}
}

func TestDynamicModelRebindsDifferentSurfaceOnEveryModelCall(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_surface_rebind"
	agenttest.CreateAgentDraft(t, database, draftID)
	stub := &surfaceHistoryModel{}
	service, err := NewService(t.Context(), database, stub)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	seedSurfaceTranscript(t, service)
	if _, err := seedTimelineVersion(
		service, t.Context(), draftID, timeline.Empty(draftID, 1), "surface_rebind_fixture", nil,
	); err != nil {
		t.Fatal(err)
	}

	ctx := withTestTurnLeaseSession(t, service, t.Context(), draftID)
	if _, err := service.react.Generate(ctx, []*schema.Message{schema.UserMessage("搜索口播台词")}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.react.Generate(ctx, []*schema.Message{schema.UserMessage("只修改时间线片段音量")}); err != nil {
		t.Fatal(err)
	}
	history := stub.snapshots()
	if len(history) != 2 {
		t.Fatalf("provider WithTools calls=%d want=2: %v", len(history), history)
	}
	if reflect.DeepEqual(history[0], history[1]) {
		t.Fatalf("不同阶段不应绑定相同工具面: %v", history)
	}
	if !containsName(history[0], "speech.search") || containsName(history[0], "timeline.update") {
		t.Fatalf("talking-head surface=%v", history[0])
	}
	if !reflect.DeepEqual(history[1], []string{
		"timeline.check",
		"timeline.delete",
		"timeline.insert",
		"timeline.inspect",
		"timeline.split",
		"timeline.update",
	}) {
		t.Fatalf("timeline-edit surface=%v", history[1])
	}
}

func TestProviderFinalExportRequestOnlyGuidesUserToUI(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_user_final_export_provider"
	agenttest.CreateAgentDraft(t, database, draftID)
	stub := &finalExportProviderSpy{}
	service, err := NewService(t.Context(), database, stub)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if _, err := seedTimelineVersion(
		service,
		rushestools.WithTimelineMutationOrigin(t.Context(), "manual"),
		draftID,
		timeline.Empty(draftID, 1),
		"user_final_export_provider_fixture",
		nil,
	); err != nil {
		t.Fatal(err)
	}

	response, err := service.react.Generate(
		withTestTurnLeaseSession(t, service, t.Context(), draftID),
		[]*schema.Message{schema.UserMessage("渲染最终视频并下载 MP4")},
	)
	if err != nil {
		t.Fatal(err)
	}
	boundTools, calls := stub.snapshot()
	if calls != 1 || !reflect.DeepEqual(boundTools, []string{"timeline.inspect"}) {
		t.Fatalf("provider calls=%d bound_tools=%v", calls, boundTools)
	}
	if !strings.Contains(response.Content, "点击导出") ||
		!strings.Contains(response.Content, "下载") {
		t.Fatalf("assistant 未引导用户使用 UI 导出: %q", response.Content)
	}
	var finalJobs, liveLeases int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM jobs WHERE kind='render_final'",
	).Scan(&finalJobs); err != nil {
		t.Fatal(err)
	}
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM agent_edit_leases WHERE draft_id=?", draftID,
	).Scan(&liveLeases); err != nil {
		t.Fatal(err)
	}
	if finalJobs != 0 || liveLeases != 0 {
		t.Fatalf("final_jobs=%d edit_leases=%d", finalJobs, liveLeases)
	}
}

func TestModelCannotExecuteRegisteredToolOutsideBoundSurface(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_surface_guard"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)

	ctx := withModelToolSurfaceSession(rushestools.WithDraftID(t.Context(), draftID))
	session := modelToolSurfaceSessionFromContext(ctx)
	session.set([]string{"asset.list_assets"})
	var beatTool einotool.InvokableTool
	for _, spec := range service.tools.Specs(true) {
		if spec.Name == "audio.analyze_beats" {
			beatTool = spec.Implementation.(einotool.InvokableTool)
			break
		}
	}
	if beatTool == nil {
		t.Fatal("audio.analyze_beats missing")
	}
	_, err = beatTool.InvokableRun(ctx, `{"asset_id":"asset_surface"}`)
	var rejection *rushestools.InterceptorRejection
	if !errors.As(err, &rejection) ||
		rejection.Data["error_code"] != string(rushestools.ErrCodeToolNotInSurface) {
		t.Fatalf("outside-surface call err=%v", err)
	}
	if !reflect.DeepEqual(rejection.Data["available_tools"], []string{"asset.list_assets"}) {
		t.Fatalf("rejection=%#v", rejection.Data)
	}

	var previewGenerateTool einotool.InvokableTool
	for _, spec := range service.tools.Specs(true) {
		if spec.Name == "preview.generate" {
			previewGenerateTool = spec.Implementation.(einotool.InvokableTool)
			break
		}
	}
	if previewGenerateTool == nil {
		t.Fatal("preview.generate missing")
	}
	_, err = previewGenerateTool.InvokableRun(ctx, `{"timeline_id":"draft:v1"}`)
	rejection = nil
	if !errors.As(err, &rejection) ||
		rejection.Data["error_code"] != string(rushestools.ErrCodeToolNotInSurface) {
		t.Fatalf("outside-surface call with failed precondition err=%v", err)
	}
}
