package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
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
		UPDATE drafts SET timeline_current_version='timeline_surface', timeline_validated=?
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
		[]string{"timeline.apply_patches", "timeline.compose_initial"},
	)

	seedSurfaceAsset(t, service, draftID)
	assertSurface(
		"读取口播台词和气口证据",
		[]string{
			"speech.transcribe", "speech.search", "audio.analyze_speech_pauses",
			"media.detect_shots", "shot.search", "timeline.compose_initial",
		},
		[]string{"timeline.edit_talking_head", "timeline.apply_patches"},
	)
	assertSurface(
		"请组装初版时间线",
		[]string{"timeline.compose_initial", "asset.list_assets", "shot.search"},
		[]string{"timeline.apply_patches"},
	)

	setSurfaceTimelineState(t, service, draftID, false)
	assertSurface(
		"只把当前时间线片段音量调低",
		[]string{"timeline.apply_patches", "timeline.inspect"},
		[]string{"timeline.edit_talking_head", "timeline.recut_to_beats"},
	)
	assertSurface(
		"完成口播气口和重说清理",
		[]string{"timeline.edit_talking_head", "speech.search", "media.detect_shots", "timeline.inspect"},
		[]string{"timeline.apply_patches", "timeline.recut_to_beats", "timeline.compose_initial"},
	)
	assertSurface(
		"根据节拍和 BGM 做卡点",
		[]string{"timeline.recut_to_beats", "audio.analyze_beats", "media.detect_shots", "shot.search"},
		[]string{"timeline.apply_patches", "timeline.edit_talking_head"},
	)
	assertSurface(
		"验证时间线后渲染预览",
		[]string{"timeline.check", "timeline.inspect", "render.preview", "render.final_mp4", "render.status"},
		[]string{"timeline.apply_patches"},
	)

	setSurfaceTimelineState(t, service, draftID, true)
	assertSurface(
		"渲染预览并导出最终 MP4",
		[]string{"render.preview", "render.final_mp4", "timeline.check"},
		[]string{"timeline.apply_patches"},
	)
	assertSurface(
		"记住我的长期偏好并更新计划",
		[]string{"memory.update", "plan.update", "interaction.confirm_action"},
		[]string{"timeline.apply_patches", "speech.search"},
	)
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
	if containsName(names, "timeline.apply_patches") {
		t.Fatalf("检索完成前 surface=%v unexpectedly exposes timeline.apply_patches", names)
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
		!containsName(names, "timeline.apply_patches") ||
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
			containsName(names, "timeline.apply_patches") {
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
			required:  "timeline.edit_talking_head",
			forbidden: []string{"timeline.apply_patches", "timeline.recut_to_beats"},
		},
		{
			name: "beat_edit", prompt: "按 BGM 卡点并找一个镜头插入",
			required:  "timeline.recut_to_beats",
			forbidden: []string{"timeline.apply_patches", "timeline.edit_talking_head"},
		},
		{
			name: "generic_timeline_edit", prompt: "在时间线里找一个海边镜头插入",
			required:  "timeline.apply_patches",
			forbidden: []string{"timeline.edit_talking_head", "timeline.recut_to_beats"},
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

func TestAutomaticContinuationPreservesTalkingHeadIntentAfterCompose(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_talking_head_continuation_surface"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	setSurfaceTimelineState(t, service, draftID, false)

	messages := []*schema.Message{
		schema.UserMessage("清理口播"),
		schema.UserMessage(
			"你等待的后台任务已到终态。\n任务：understand\n状态：成功\n" +
				"这是原任务的自动续跑，不是新的用户请求。请继续。",
		),
		schema.ToolMessage(
			`{"status":"succeeded"}`,
			"call_compose_after_understand",
			schema.WithToolName("timeline.compose_initial"),
		),
	}
	specs, err := selectModelToolSurface(
		rushestools.WithDraftID(t.Context(), draftID),
		service.tools,
		messages,
	)
	if err != nil {
		t.Fatal(err)
	}
	names := surfaceNames(specs)
	if !containsName(names, "timeline.edit_talking_head") ||
		containsName(names, "timeline.apply_patches") ||
		containsName(names, "timeline.compose_initial") {
		t.Fatalf("后台续跑后的口播 surface=%v", names)
	}
}

func TestAutomaticRenderContinuationUsesPersistedPreviewWithoutToolTrace(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_render_continuation_surface"
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
			schema.UserMessage("剪掉开头三秒，渲染预览并检查黑帧"),
			schema.UserMessage(
				"你等待的后台任务已到终态。\n任务：render_preview\n状态：成功\n" +
					"这是原任务的自动续跑，不是新的用户请求。请继续。",
			),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	names := surfaceNames(specs)
	if !containsName(names, "preview.check") ||
		containsName(names, "timeline.apply_patches") {
		t.Fatalf("真实跨回合续跑 surface=%v", names)
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
		if !containsName(names, "timeline.apply_patches") ||
			!containsName(names, "timeline.check") ||
			containsName(names, "render.final_mp4") {
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

	for _, prompt := range []string{
		"帮我剪辑这些素材",
		"导出最终成片",
		"渲染预览并做黑帧质检",
	} {
		specs, selectErr := selectModelToolSurface(ctx, service.tools, []*schema.Message{
			schema.UserMessage(prompt),
		})
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		names := surfaceNames(specs)
		if !containsName(names, "timeline.compose_initial") ||
			containsName(names, "timeline.inspect") {
			t.Errorf("%q surface=%v want discovery prerequisite tools", prompt, names)
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
	if !containsName(names, "render.preview") || containsName(names, "preview.check") {
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
			name: "compose", prompt: "请组装初版时间线",
			toolName: "timeline.compose_initial",
			required: "timeline.apply_patches", forbidden: "timeline.compose_initial",
		},
		{
			name: "talking_head_after_compose", prompt: "完成口播气口和重说清理",
			toolName: "timeline.compose_initial",
			required: "timeline.edit_talking_head", forbidden: "timeline.apply_patches",
		},
		{
			name: "talking_head", prompt: "完成口播气口和重说清理",
			toolName: "timeline.edit_talking_head",
			required: "timeline.check", forbidden: "timeline.edit_talking_head",
		},
		{
			name: "beat", prompt: "根据节拍和 BGM 做卡点",
			toolName: "timeline.recut_to_beats",
			required: "timeline.check", forbidden: "timeline.recut_to_beats",
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

func TestCompositeEditThenRenderRequestStartsWithEditSurface(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_composite_edit_render_surface"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	setSurfaceTimelineState(t, service, draftID, false)
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	tests := []struct {
		prompt   string
		editTool string
	}{
		{"剪掉开头三秒然后导出 MP4", "timeline.apply_patches"},
		{"清理口播气口和重说后导出", "timeline.edit_talking_head"},
		{"按 BGM 卡点后渲染预览", "timeline.recut_to_beats"},
	}
	for _, test := range tests {
		t.Run(test.editTool, func(t *testing.T) {
			messages := []*schema.Message{schema.UserMessage(test.prompt)}
			specs, selectErr := selectModelToolSurface(ctx, service.tools, messages)
			if selectErr != nil {
				t.Fatal(selectErr)
			}
			names := surfaceNames(specs)
			if !containsName(names, test.editTool) ||
				containsName(names, "render.preview") ||
				containsName(names, "render.final_mp4") {
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
				(test.editTool != "timeline.apply_patches" && containsName(names, test.editTool)) {
				t.Fatalf("编辑完成后 surface=%v", names)
			}
		})
	}
}

func TestCompositeEditRenderAndPreviewCheckPreservesStageOrder(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_composite_edit_preview_check"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
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
		"先调用 timeline.apply_patches，再调用 preview.check",
	} {
		specs, selectErr := selectModelToolSurface(ctx, service.tools, []*schema.Message{
			schema.UserMessage(prompt),
		})
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		names := surfaceNames(specs)
		if !containsName(names, "timeline.apply_patches") ||
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
		schema.WithToolName("timeline.apply_patches"),
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
			!containsName(names, "timeline.apply_patches") {
			t.Fatalf("unrelated failure messages=%v surface=%v", messages, names)
		}
	}

	specs, err := selectModelToolSurface(ctx, service.tools, []*schema.Message{
		user,
		success,
		schema.ToolMessage(
			`{"status":"failed","error_code":"invalid_arguments"}`,
			"call_apply_failure",
			schema.WithToolName("timeline.apply_patches"),
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	names := surfaceNames(specs)
	if !containsName(names, "timeline.apply_patches") {
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
			schema.WithToolName("timeline.apply_patches"),
		),
	}

	specs, err := selectModelToolSurface(ctx, service.tools, messages)
	if err != nil {
		t.Fatal(err)
	}
	names := surfaceNames(specs)
	if !containsName(names, "timeline.apply_patches") ||
		!containsName(names, "timeline.inspect") ||
		!containsName(names, "timeline.check") ||
		containsName(names, "render.preview") {
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
	if !containsName(names, "render.preview") ||
		containsName(names, "timeline.apply_patches") {
		t.Fatalf("显式验证后 surface=%v", names)
	}
}

func TestSuccessfulPreviewAdvancesCompositeWorkflowToInspection(t *testing.T) {
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
			schema.WithToolName("timeline.apply_patches"),
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
			schema.WithToolName("render.preview"),
		),
	))
	if err != nil {
		t.Fatal(err)
	}
	if names := surfaceNames(specs); !containsName(names, "render.preview") ||
		containsName(names, "preview.check") {
		t.Fatalf("preview failure surface=%v", names)
	}

	specs, err = selectModelToolSurface(ctx, service.tools, append(base,
		schema.ToolMessage(
			`{"status":"queued","data":{"job_id":"preview_job","job_status":"pending"}}`,
			"call_preview_success",
			schema.WithToolName("render.preview"),
		),
	))
	if err != nil {
		t.Fatal(err)
	}
	if names := surfaceNames(specs); !containsName(names, "preview.check") ||
		containsName(names, "render.preview") {
		t.Fatalf("preview success surface=%v", names)
	}
}

func TestExplicitCompositeEditThenRenderStartsWithEditOnValidatedTimeline(t *testing.T) {
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
			schema.UserMessage("先调用 timeline.apply_patches，再调用 render.final_mp4"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	names := surfaceNames(specs)
	if !containsName(names, "timeline.apply_patches") ||
		containsName(names, "render.final_mp4") {
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
				schema.WithToolName("timeline.apply_patches"),
			),
			schema.ToolMessage(
				`{"status":"failed","observation":"补丁重试失败"}`,
				"call_apply_failed",
				schema.WithToolName("timeline.apply_patches"),
			),
		})
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		names := surfaceNames(specs)
		if !containsName(names, "timeline.apply_patches") {
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
			containsName(names, "timeline.apply_patches") {
			t.Fatalf("最新镜头检索为空后 surface=%v", names)
		}
	})
}

func TestCompositeSpecializedAndGenericEditsFinishBeforeRender(t *testing.T) {
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
			`{"status":"succeeded"}`,
			"call_talking_head",
			schema.WithToolName("timeline.edit_talking_head"),
		),
	}
	specs, err := selectModelToolSurface(ctx, service.tools, messages)
	if err != nil {
		t.Fatal(err)
	}
	names := surfaceNames(specs)
	if !containsName(names, "timeline.apply_patches") ||
		containsName(names, "timeline.edit_talking_head") {
		t.Fatalf("专用编辑完成后 surface=%v", names)
	}

	specs, err = selectModelToolSurface(ctx, service.tools, append(messages,
		schema.ToolMessage(
			`{"status":"succeeded"}`,
			"call_apply_patches",
			schema.WithToolName("timeline.apply_patches"),
		),
	))
	if err != nil {
		t.Fatal(err)
	}
	names = surfaceNames(specs)
	if !containsName(names, "timeline.check") ||
		!containsName(names, "timeline.apply_patches") {
		t.Fatalf("全部编辑完成后 surface=%v", names)
	}

	setSurfaceTimelineState(t, service, draftID, true)
	specs, err = selectModelToolSurface(ctx, service.tools, append(messages,
		schema.ToolMessage(
			`{"status":"succeeded"}`,
			"call_apply_patches",
			schema.WithToolName("timeline.apply_patches"),
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
	if !containsName(names, "render.preview") || containsName(names, "timeline.apply_patches") {
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
	if !containsName(names, "plan.update") || containsName(names, "timeline.apply_patches") {
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
	if !containsName(names, "timeline.apply_patches") || containsName(names, "memory.update") {
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
	if !containsName(names, "plan.update") || containsName(names, "timeline.apply_patches") {
		t.Fatalf("最新计划失败 surface=%v", names)
	}
}

func TestSuccessfulControlActionAdvancesCompositeRequest(t *testing.T) {
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
		{"更新计划，然后剪掉开头三秒并导出", "plan.update", "timeline.apply_patches"},
		{"记住我偏好短片，然后剪掉开头三秒", "memory.update", "timeline.apply_patches"},
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
		if !containsName(names, test.wantTool) || containsName(names, "memory.update") {
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
			containsName(names, "timeline.apply_patches") {
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
	if !containsName(beforeNames, "timeline.compose_initial") ||
		containsName(beforeNames, "timeline.apply_patches") {
		t.Fatalf("初始 surface=%v", beforeNames)
	}

	setSurfaceTimelineState(t, service, draftID, true)
	afterComposeMessages := append(messages,
		schema.ToolMessage(
			`{"status":"succeeded","observation":"初版时间线已建立"}`,
			"call_compose",
			schema.WithToolName("timeline.compose_initial"),
		),
	)
	after, err := selectModelToolSurface(ctx, service.tools, afterComposeMessages)
	if err != nil {
		t.Fatal(err)
	}
	afterNames := surfaceNames(after)
	if !containsName(afterNames, "timeline.apply_patches") ||
		containsName(afterNames, "timeline.compose_initial") {
		t.Fatalf("状态推进后 surface=%v", afterNames)
	}

	setSurfaceTimelineState(t, service, draftID, false)
	afterEditMessages := append(messages,
		schema.ToolMessage(
			`{"status":"succeeded","observation":"时间线补丁已应用"}`,
			"call_apply",
			schema.WithToolName("timeline.apply_patches"),
		),
	)
	afterEdit, err := selectModelToolSurface(ctx, service.tools, afterEditMessages)
	if err != nil {
		t.Fatal(err)
	}
	afterEditNames := surfaceNames(afterEdit)
	if !containsName(afterEditNames, "timeline.check") ||
		!containsName(afterEditNames, "timeline.apply_patches") {
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
	if !containsName(afterValidationNames, "render.preview") ||
		!containsName(afterValidationNames, "render.final_mp4") ||
		containsName(afterValidationNames, "timeline.apply_patches") {
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
	setSurfaceTimelineState(t, service, draftID, false)

	ctx := withModelToolSurfaceSession(rushestools.WithDraftID(t.Context(), draftID))
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
	if !containsName(history[0], "speech.search") || containsName(history[0], "timeline.apply_patches") {
		t.Fatalf("talking-head surface=%v", history[0])
	}
	if !reflect.DeepEqual(history[1], []string{"timeline.apply_patches", "timeline.check", "timeline.inspect"}) {
		t.Fatalf("timeline-edit surface=%v", history[1])
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

	var finalRenderTool einotool.InvokableTool
	for _, spec := range service.tools.Specs(true) {
		if spec.Name == "render.final_mp4" {
			finalRenderTool = spec.Implementation.(einotool.InvokableTool)
			break
		}
	}
	if finalRenderTool == nil {
		t.Fatal("render.final_mp4 missing")
	}
	_, err = finalRenderTool.InvokableRun(ctx, `{}`)
	rejection = nil
	if !errors.As(err, &rejection) ||
		rejection.Data["error_code"] != string(rushestools.ErrCodeToolNotInSurface) {
		t.Fatalf("outside-surface call with failed precondition err=%v", err)
	}
}
