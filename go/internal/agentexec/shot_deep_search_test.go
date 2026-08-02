package agentexec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/media"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type deepSearchVisionStub struct {
	calls    int
	messages []*schema.Message
}

func (stub *deepSearchVisionStub) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (stub *deepSearchVisionStub) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	stub.calls++
	stub.messages = append(stub.messages, messages...)
	if len(messages) == 1 && len(messages[0].UserInputMultiContent) > 0 &&
		strings.Contains(messages[0].UserInputMultiContent[0].Text, "分析 facets：appearance_detail,text_ocr") {
		return schema.AssistantMessage(`{
  "observations":[
    {"facet":"appearance_detail","statement":"屏幕边框内可见小号型号文字","frame_ids":["f_shot_action_10"]},
    {"facet":"text_ocr","statement":"屏幕清晰显示型号 ZX-9","frame_ids":["f_shot_action_10"]}
  ],
  "requirements":[],"exclusions":[],"preferences":[]
}`, nil), nil
	}
	return schema.AssistantMessage(`{
  "observations":[
    {"facet":"appearance","statement":"画面主体为单个人物","frame_ids":["f_shot_action_11"]},
    {"facet":"temporal_action","statement":"人物从左向右连续旋转","frame_ids":["f_shot_action_11","f_shot_action_79"]}
  ],
  "requirements":[{"id":"r0","status":"observed","observation":"多帧显示人物姿态连续改变","frame_ids":["f_shot_action_11","f_shot_action_34","f_shot_action_79"]}],
  "exclusions":[{"id":"e0","status":"refuted","observation":"各证据帧均未显示降雨","frame_ids":["f_shot_action_11"]}],
  "preferences":[{"id":"p0","status":"observed","observation":"首尾姿态差异明显","frame_ids":["f_shot_action_11","f_shot_action_79"]}]
}`, nil), nil
}

func (stub *deepSearchVisionStub) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := stub.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestShotDeepSearchPersistsUniversalFactsReusesFramesAndInvalidatesStaleSnapshot(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_shot_deep"
	const actionAssetID = "asset_action"
	const otherAssetID = "asset_other"
	const actionSnapshotID = "snapshot_action_v1"
	actionHash := strings.Repeat("b", 64)
	otherHash := strings.Repeat("c", 64)
	agenttest.CreateAgentDraft(t, database, draftID)

	source := filepath.Join(database.Paths.Temporary, "deep-search-source.mp4")
	if _, err := media.RunCommand(
		t.Context(), "ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30:duration=3",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", source,
	); err != nil {
		t.Fatal(err)
	}
	seedSearchAsset(t, database, draftID, actionAssetID, actionHash, "action.mp4", "Broll")
	seedSearchAsset(t, database, draftID, otherAssetID, otherHash, "other.mp4", "Broll")
	if _, err := database.Write().Exec(
		"UPDATE assets SET reference_path=? WHERE asset_id IN (?,?)", source, actionAssetID, otherAssetID,
	); err != nil {
		t.Fatal(err)
	}
	seedSearchIndex(t, database, actionAssetID, actionHash, actionSnapshotID, 1, "b_roll", []searchShotFixture{{
		id: "shot_action", startFrame: 0, endFrame: 90,
		description: "普通室内人物镜头", subjects: []string{"人物"}, quality: map[string]any{"label": "usable"},
	}})
	seedSearchIndex(t, database, otherAssetID, otherHash, "snapshot_other_v1", 1, "b_roll", []searchShotFixture{{
		id: "shot_other", startFrame: 0, endFrame: 90,
		description: "另一个镜头", quality: map[string]any{"label": "usable"},
	}})

	vision := &deepSearchVisionStub{}
	executor, err := newTestExecutor(t.Context(), database, vision)
	if err != nil {
		t.Fatal(err)
	}
	progressStages := []string{}
	executor.progress = func(_ string, event map[string]any) {
		if stage, ok := event["stage"].(string); ok {
			progressStages = append(progressStages, stage)
		}
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	searchRaw, err := executor.ExecuteTool(ctx, "shot.search", rushestools.ShotSearchInput{Query: "普通人物", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	search := searchRaw.(rushestools.ShotSearchResult)
	if search.Status != string(rushestools.StatusSucceeded) || search.IndexSnapshotID == "" {
		t.Fatalf("search=%#v", search)
	}
	progressStages = nil

	callDeep := func(input rushestools.ShotDeepSearchInput) rushestools.ShotDeepSearchResult {
		t.Helper()
		raw, callErr := executor.ExecuteTool(ctx, "shot.deep_search", input)
		if callErr != nil {
			t.Fatal(callErr)
		}
		return raw.(rushestools.ShotDeepSearchResult)
	}
	invalidCriteria := callDeep(rushestools.ShotDeepSearchInput{
		Query: "动作", IndexSnapshotID: search.IndexSnapshotID,
		CandidateShots: []rushestools.ShotRefInput{{AssetID: actionAssetID, ShotID: "shot_action"}},
		Requirements:   []string{"重复", "重复"},
	})
	if invalidCriteria.ErrorCode != string(rushestools.ErrCodeShotDeepInputInvalid) || vision.calls != 0 {
		t.Fatalf("invalidCriteria=%#v calls=%d", invalidCriteria, vision.calls)
	}
	unknown := callDeep(rushestools.ShotDeepSearchInput{
		Query: "动作", IndexSnapshotID: search.IndexSnapshotID,
		CandidateShots: []rushestools.ShotRefInput{{AssetID: actionAssetID, ShotID: "shot_missing"}},
	})
	if unknown.ErrorCode != string(rushestools.ErrCodeShotRefNotFound) || vision.calls != 0 {
		t.Fatalf("unknown=%#v calls=%d", unknown, vision.calls)
	}
	mismatch := callDeep(rushestools.ShotDeepSearchInput{
		Query: "动作", IndexSnapshotID: search.IndexSnapshotID,
		CandidateShots: []rushestools.ShotRefInput{{AssetID: actionAssetID, ShotID: "shot_other"}},
	})
	if mismatch.ErrorCode != string(rushestools.ErrCodeShotRefAssetMismatch) || vision.calls != 0 {
		t.Fatalf("mismatch=%#v calls=%d", mismatch, vision.calls)
	}

	input := rushestools.ShotDeepSearchInput{
		Query: "检查连续旋转动作", IndexSnapshotID: search.IndexSnapshotID,
		CandidateShots: []rushestools.ShotRefInput{{AssetID: actionAssetID, ShotID: "shot_action"}},
		Requirements:   []string{"人物完成连续旋转"}, Exclusions: []string{"下雨"},
		Preferences: []string{"动作幅度大"}, ReturnTopK: 1,
	}
	noVision, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	noVisionRaw, err := noVision.ExecuteTool(ctx, "shot.deep_search", input)
	if err != nil {
		t.Fatal(err)
	}
	if unavailable := noVisionRaw.(rushestools.ShotDeepSearchResult); unavailable.ErrorCode != string(rushestools.ErrCodeShotDeepVisionMissing) {
		t.Fatalf("unavailable=%#v", unavailable)
	}
	first := callDeep(input)
	if first.Status != string(rushestools.StatusSucceeded) || first.CacheHit ||
		first.NewFrameCount != 7 || first.ReusedFrameCount != 0 || len(first.Candidates) != 1 ||
		first.Candidates[0].Verification != "match" || vision.calls != 1 {
		t.Fatalf("first=%#v calls=%d", first, vision.calls)
	}
	if len(progressStages) != 3 || progressStages[0] != "deep_frame_plan" ||
		progressStages[1] != "deep_frame_extract" || progressStages[2] != "deep_persist" {
		t.Fatalf("progress=%#v", progressStages)
	}
	for _, frame := range first.Candidates[0].FrameEvidence {
		if frame.SourceFrame == 45 || !frame.NewlyAdded || frame.SourceFrame < 0 || frame.SourceFrame >= 90 {
			t.Fatalf("新增帧越过边界或复用了基础代表帧: %#v", frame)
		}
	}
	if len(vision.messages) != 1 || len(vision.messages[0].UserInputMultiContent) != 15 ||
		!strings.Contains(vision.messages[0].UserInputMultiContent[0].Text, "temporal_action") {
		t.Fatalf("VLM 输入未包含有序七帧与 facet: %#v", vision.messages)
	}

	var analysisCount, objectCount, snapshotCount, shotCount int
	for query, destination := range map[string]*int{
		"SELECT COUNT(*) FROM asset_analyses WHERE analysis_type='shot_deep_facts'": &analysisCount,
		"SELECT COUNT(*) FROM objects":                                              &objectCount,
		"SELECT COUNT(*) FROM shot_index_snapshots":                                 &snapshotCount,
		"SELECT COUNT(*) FROM shots":                                                &shotCount,
	} {
		if err := database.Read().QueryRow(query).Scan(destination); err != nil {
			t.Fatal(err)
		}
	}
	if analysisCount != 1 || objectCount != 7 || snapshotCount != 2 || shotCount != 2 {
		t.Fatalf("analysis=%d objects=%d snapshots=%d shots=%d", analysisCount, objectCount, snapshotCount, shotCount)
	}
	var persisted string
	if err := database.Read().QueryRow(`
		SELECT result_json FROM asset_analyses WHERE analysis_type='shot_deep_facts'`,
	).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	for _, querySpecific := range []string{input.Query, input.Requirements[0], input.Exclusions[0], input.Preferences[0], "verification", "score"} {
		if strings.Contains(persisted, querySpecific) {
			t.Fatalf("查询特定判断被写入通用事实: marker=%q json=%s", querySpecific, persisted)
		}
	}
	if !strings.Contains(persisted, "人物从左向右连续旋转") || !strings.Contains(persisted, "temporal_action") {
		t.Fatalf("通用事实未持久化: %s", persisted)
	}

	// 新 Executor 模拟进程重启：复用持久化 facet 帧，只重做当前 query 的逐项核验。
	restarted, err := newTestExecutor(t.Context(), database, vision)
	if err != nil {
		t.Fatal(err)
	}
	reusedRaw, err := restarted.ExecuteTool(ctx, "shot.deep_search", input)
	if err != nil {
		t.Fatal(err)
	}
	reused := reusedRaw.(rushestools.ShotDeepSearchResult)
	if !reused.CacheHit || reused.NewFrameCount != 0 || reused.ReusedFrameCount != 7 || vision.calls != 2 {
		t.Fatalf("reused=%#v calls=%d", reused, vision.calls)
	}
	for _, frame := range reused.Candidates[0].FrameEvidence {
		if frame.NewlyAdded {
			t.Fatalf("持久化 frame 被误报为新增: %#v", frame)
		}
	}
	if err := database.Read().QueryRow(
		"SELECT COUNT(*) FROM asset_analyses WHERE analysis_type='shot_deep_facts'",
	).Scan(&analysisCount); err != nil || analysisCount != 1 {
		t.Fatalf("analysisCount=%d err=%v", analysisCount, err)
	}
	if _, err := restarted.ExecuteTool(ctx, "shot.deep_search", input); err != nil || vision.calls != 2 {
		t.Fatalf("同 Executor 精确 query cache 未命中: calls=%d err=%v", vision.calls, err)
	}
	ocrInput := rushestools.ShotDeepSearchInput{
		Query: "读取屏幕型号细节文字", IndexSnapshotID: search.IndexSnapshotID,
		CandidateShots: []rushestools.ShotRefInput{{AssetID: actionAssetID, ShotID: "shot_action"}},
	}
	ocrRaw, err := restarted.ExecuteTool(ctx, "shot.deep_search", ocrInput)
	if err != nil {
		t.Fatal(err)
	}
	ocr := ocrRaw.(rushestools.ShotDeepSearchResult)
	if ocr.Status != string(rushestools.StatusSucceeded) || ocr.CacheHit ||
		ocr.NewFrameCount != 7 || ocr.ReusedFrameCount != 2 || vision.calls != 3 ||
		!containsString(ocr.Candidates[0].DeepCoverage, "appearance_detail") ||
		!containsString(ocr.Candidates[0].DeepCoverage, "text_ocr") {
		t.Fatalf("ocr=%#v calls=%d", ocr, vision.calls)
	}
	if err := database.Read().QueryRow(
		"SELECT COUNT(*) FROM asset_analyses WHERE analysis_type='shot_deep_facts'",
	).Scan(&analysisCount); err != nil || analysisCount != 2 {
		t.Fatalf("progressive analysisCount=%d err=%v", analysisCount, err)
	}
	if err := database.Read().QueryRow("SELECT COUNT(*) FROM objects").Scan(&objectCount); err != nil || objectCount != 14 {
		t.Fatalf("progressive objectCount=%d err=%v", objectCount, err)
	}

	benefitRaw, err := restarted.ExecuteTool(ctx, "shot.search", rushestools.ShotSearchInput{
		Query: "屏幕型号 ZX-9", AssetIDs: []string{actionAssetID}, TopK: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	benefit := benefitRaw.(rushestools.ShotSearchResult)
	if len(benefit.Shots) != 1 || benefit.Shots[0].ShotID != "shot_action" ||
		!containsString(benefit.Shots[0].DeepCoverage, "temporal_action") ||
		!containsString(benefit.Shots[0].DeepCoverage, "text_ocr") ||
		!strings.Contains(benefit.Shots[0].Description, "屏幕清晰显示型号 ZX-9") {
		t.Fatalf("deep facts 未反哺 shot.search: %#v", benefit)
	}

	if _, err := database.Write().Exec(
		"UPDATE shot_index_snapshots SET status='superseded' WHERE index_snapshot_id=?", actionSnapshotID,
	); err != nil {
		t.Fatal(err)
	}
	seedSearchIndex(t, database, actionAssetID, actionHash, "snapshot_action_v2", 2, "b_roll", []searchShotFixture{{
		id: "shot_action", startFrame: 0, endFrame: 89, description: "更新边界",
		quality: map[string]any{"label": "usable"},
	}})
	staleRaw, err := restarted.ExecuteTool(ctx, "shot.deep_search", input)
	if err != nil {
		t.Fatal(err)
	}
	stale := staleRaw.(rushestools.ShotDeepSearchResult)
	if stale.Status != string(rushestools.StatusFailed) ||
		stale.ErrorCode != string(rushestools.ErrCodeShotIndexSnapshotStale) || vision.calls != 3 {
		t.Fatalf("stale=%#v calls=%d", stale, vision.calls)
	}

	for _, frame := range first.Candidates[0].FrameEvidence {
		path, err := database.Paths.ObjectPath(frame.ObjectHash)
		if err != nil {
			t.Fatal(err)
		}
		if stat, err := os.Stat(path); err != nil || stat.Size() != frame.ObjectSize {
			t.Fatalf("深搜帧对象未保活: path=%s stat=%#v err=%v", path, stat, err)
		}
	}
}
