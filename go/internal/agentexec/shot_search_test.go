package agentexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

type searchShotFixture struct {
	id                 string
	startFrame         int
	endFrame           int
	description        string
	tags               []string
	subjects           []string
	actions            []string
	setting            []string
	shotScale          string
	composition        string
	lighting           []string
	mood               []string
	editHints          []string
	quality            map[string]any
	boundaryConfidence *float64
	deepCoverage       []string
}

func TestShotSearchReadsPersistentSnapshotAndDoesNotMutateBusinessTruth(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_snapshot_search"
	agenttest.CreateAgentDraft(t, database, draftID)
	seedSearchAsset(t, database, draftID, "asset_beach", "hash_beach", "海边舞蹈.mov", "Broll")
	seedSearchAsset(t, database, draftID, "asset_city", "hash_city", "城市夜景.mov", "Broll")
	confidence := 0.96
	seedSearchIndex(t, database, "asset_beach", "hash_beach", "snapshot_beach_v1", 1, "b_roll", []searchShotFixture{
		{
			id: "shot_beach_wide", startFrame: 0, endFrame: 90,
			description: "落日海边，一名人物在海浪前舞蹈，环境远景",
			tags:        []string{"落日", "海浪"}, subjects: []string{"人物"}, actions: []string{"舞蹈"},
			setting: []string{"海边"}, shotScale: "远景", composition: "人物居中，海岸线横向延伸",
			lighting: []string{"夕阳"}, mood: []string{"舒展"}, editHints: []string{"建立镜头"},
			quality:            map[string]any{"label": "usable", "overexposed_ratio": 0.01, "sharpness": 420.0},
			boundaryConfidence: &confidence,
		},
		{
			id: "shot_beach_detail", startFrame: 90, endFrame: 150,
			description: "人物脚步踩过沙滩的局部细节", tags: []string{"沙滩", "脚步"},
			subjects: []string{"脚步"}, actions: []string{"行走"}, setting: []string{"海滩"},
			shotScale: "特写", composition: "低机位", lighting: []string{"黄昏"},
			quality: map[string]any{"label": "usable", "sharpness": 380.0},
		},
	})
	seedSearchIndex(t, database, "asset_city", "hash_city", "snapshot_city_v1", 1, "b_roll", []searchShotFixture{{
		id: "shot_city", startFrame: 0, endFrame: 120, description: "夜晚城市天际线",
		tags: []string{"城市", "夜景"}, subjects: []string{"建筑"}, actions: []string{"车流"},
		setting: []string{"城市"}, shotScale: "远景", composition: "对称构图",
		lighting: []string{"霓虹"}, quality: map[string]any{"label": "usable"},
	}})

	before := searchBusinessTruthCounts(t, database)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	input := rushestools.ShotSearchInput{Query: "海边落日人物舞蹈的远景", TopK: 5}
	raw, err := exec.ExecuteTool(ctx, "shot.search", input)
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ShotSearchResult)
	if result.Status != string(rushestools.StatusSucceeded) || !result.SearchReady ||
		result.IndexSnapshotID == "" || result.SynonymVersion != shotSearchSynonymVersion ||
		len(result.Shots) == 0 || result.Shots[0].ShotID != "shot_beach_wide" {
		t.Fatalf("search result=%#v", result)
	}
	first := result.Shots[0]
	if first.IndexSnapshotID != result.IndexSnapshotID || first.AssetID != "asset_beach" ||
		first.SourceStartFrame != 0 || first.SourceEndFrame != 90 || first.BoundaryVersion != 1 ||
		len(first.MatchedTerms) == 0 || len(first.MatchEvidence) == 0 {
		t.Fatalf("first candidate=%#v", first)
	}
	after := searchBusinessTruthCounts(t, database)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("shot.search 改写业务真值: before=%v after=%v", before, after)
	}

	repeatedRaw, err := exec.ExecuteTool(ctx, "shot.search", input)
	if err != nil {
		t.Fatal(err)
	}
	repeated := repeatedRaw.(rushestools.ShotSearchResult)
	if repeated.IndexSnapshotID != result.IndexSnapshotID ||
		!reflect.DeepEqual(shotIDs(repeated.Shots), shotIDs(result.Shots)) {
		t.Fatalf("同一快照重复检索漂移: first=%#v repeated=%#v", result, repeated)
	}
	var vectorTables int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND (name LIKE 'vector%' OR name LIKE '%embedding%')`,
	).Scan(&vectorTables); err != nil {
		t.Fatal(err)
	}
	if vectorTables != 0 {
		t.Fatalf("无 embedding 检索不应生成向量表: %d", vectorTables)
	}
}

func TestShotSearchDeterministicRankingHelperEdges(t *testing.T) {
	t.Parallel()

	if !overlapsAny(sourceRange{startFrame: 10, endFrame: 20}, []sourceRange{{startFrame: 19, endFrame: 30}}) {
		t.Fatal("相交的素材区间应被识别")
	}
	if overlapsAny(sourceRange{startFrame: 10, endFrame: 20}, []sourceRange{{startFrame: 20, endFrame: 30}}) {
		t.Fatal("首尾相接的半开区间不应被视为相交")
	}
	if got := stringValue(nil); got != "" {
		t.Fatalf("nil stringValue=%q", got)
	}
	if !containsDigit("take-2") || containsDigit("take-two") {
		t.Fatal("数字检测结果不符合预期")
	}

	fields := []weightedSearchField{{name: "标签", value: "海边 落日", weight: 1}}
	if !matchesAnyTagQuery([]shotSearchQuery{buildShotSearchQuery("海滩")}, fields) {
		t.Fatal("同义词标签查询应命中")
	}
	if matchesAnyTagQuery([]shotSearchQuery{buildShotSearchQuery("城市")}, fields) {
		t.Fatal("无关标签查询不应命中")
	}

	if got := shotQualityPenalty(map[string]any{
		"overexposed_ratio": 0.2,
		"sharpness":         50.0,
	}); got != 0.065 {
		t.Fatalf("quality penalty=%v, want 0.065", got)
	}
	if got := shotQualityPenalty(map[string]any{}); got != 0 {
		t.Fatalf("empty quality penalty=%v", got)
	}

	candidates := []rushestools.ShotCandidate{
		{AssetID: "asset_b", ShotID: "shot_a", Score: 0.8},
		{AssetID: "asset_a", ShotID: "shot_b", Score: 0.8},
		{AssetID: "asset_a", ShotID: "shot_a", Score: 0.8},
		{AssetID: "asset_z", ShotID: "shot_z", Score: 0.9},
	}
	sortShotCandidates(candidates)
	if got, want := shotIDs(candidates), []string{
		"asset_z:shot_z", "asset_a:shot_a", "asset_b:shot_a", "asset_a:shot_b",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stable candidate order=%v, want %v", got, want)
	}
	if !shotCandidateLess(
		rushestools.ShotCandidate{AssetID: "asset_a", ShotID: "same"},
		rushestools.ShotCandidate{AssetID: "asset_b", ShotID: "same"},
	) {
		t.Fatal("同一 shot_id 应以 asset_id 稳定决胜")
	}
	if overlap := shotSemanticOverlap(rushestools.ShotCandidate{}, rushestools.ShotCandidate{}); overlap != 0 {
		t.Fatalf("空语义集合 overlap=%v", overlap)
	}

	if got, want := compactStringsStable([]string{"", " a ", "a", "b"}, 1), []string{"a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("compact stable=%v, want %v", got, want)
	}
	if got := WeightedSemanticMatchScore(nil, "anything"); got != 0.5 {
		t.Fatalf("empty weighted tokens score=%v", got)
	}
	if got := SemanticMatchScore(nil, "anything"); got != 0.5 {
		t.Fatalf("empty semantic tokens score=%v", got)
	}
	weights := map[string]float64{
		"v2":   4,
		"海":    0.15,
		"海边":   1,
		"wide": 2,
		"go":   0.5,
	}
	for token, want := range weights {
		if got := semanticTokenWeight(token); got != want {
			t.Fatalf("semanticTokenWeight(%q)=%v, want %v", token, got, want)
		}
	}
}

func TestShotSearchRejectsInvalidInputAndPreservesBarrierCancellation(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_search_validation"
	agenttest.CreateAgentDraft(t, database, draftID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}

	invalidInputs := []rushestools.ShotSearchInput{
		{MinDurationFrames: -1},
		{MaxDurationFrames: -1},
		{MinDurationFrames: 20, MaxDurationFrames: 10},
		{TopK: -1},
		{TopK: shotSearchMaximumTopK + 1},
	}
	for _, input := range invalidInputs {
		if _, err := exec.toolSearchShots(t.Context(), draftID, input); err == nil {
			t.Fatalf("invalid input should fail: %#v", input)
		}
	}
	if _, err := exec.freezeShotAssets(t.Context(), draftID, nil); err == nil {
		t.Fatal("空素材集不应进入检索")
	}

	seedSearchAsset(t, database, draftID, "asset_pending_cancel", "hash_pending_cancel", "pending.mp4", "Broll")
	frozen, err := exec.freezeShotAssets(
		t.Context(), draftID, []string{"asset_pending_cancel", "asset_pending_cancel"},
	)
	if err != nil || len(frozen) != 1 {
		t.Fatalf("重复 asset_id 应稳定去重: frozen=%#v err=%v", frozen, err)
	}
	if _, err := exec.freezeShotAssets(t.Context(), draftID, []string{"asset_missing"}); err == nil {
		t.Fatal("未知 asset_id 应被拒绝")
	}
	used, err := exec.usedShotRanges(t.Context(), draftID, true)
	if err != nil || len(used) != 0 {
		t.Fatalf("无时间线时 used ranges=%v err=%v", used, err)
	}

	seedBaseIndexJob(t, database, "asset_pending_cancel", "hash_pending_cancel", "pending")
	exec.jobPollInterval = time.Hour
	exec.jobWaitTimeout = time.Hour
	ctx, cancel := context.WithCancel(t.Context())
	exec.progress = func(_ string, _ map[string]any) { cancel() }
	if _, _, _, _, err := exec.awaitShotSearchReady(ctx, draftID, frozen); !errors.Is(err, context.Canceled) {
		t.Fatalf("barrier cancellation err=%v, want context.Canceled", err)
	}
}

func TestShotSearchWaitsForFrozenSetAndDoesNotLeakPartialResults(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_search_barrier"
	agenttest.CreateAgentDraft(t, database, draftID)
	seedSearchAsset(t, database, draftID, "asset_ready", "hash_ready", "ready.mp4", "Broll")
	seedSearchAsset(t, database, draftID, "asset_pending", "hash_pending", "pending.mp4", "Broll")
	seedSearchIndex(t, database, "asset_ready", "hash_ready", "snapshot_ready", 1, "b_roll", []searchShotFixture{{
		id: "shot_ready", startFrame: 0, endFrame: 60, description: "已就绪的室内产品镜头",
		tags: []string{"产品"}, subjects: []string{"产品"}, actions: []string{"展示"},
		setting: []string{"室内"}, shotScale: "中景", composition: "居中", quality: map[string]any{"label": "usable"},
	}})
	seedBaseIndexJob(t, database, "asset_pending", "hash_pending", "pending")

	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	exec.jobPollInterval = 2 * time.Millisecond
	exec.jobWaitTimeout = 30 * time.Millisecond
	progress := []map[string]any{}
	exec.progress = func(_ string, event map[string]any) {
		progress = append(progress, event)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	raw, err := exec.ExecuteTool(ctx, "shot.search", rushestools.ShotSearchInput{Query: "产品"})
	if err != nil {
		t.Fatal(err)
	}
	pending := raw.(rushestools.ShotSearchResult)
	if pending.Status != string(rushestools.StatusFailed) ||
		pending.ErrorCode != string(rushestools.ErrCodeShotIndexNotReady) || pending.SearchReady ||
		len(pending.Shots) != 0 || !reflect.DeepEqual(pending.ReadyAssetIDs, []string{"asset_ready"}) ||
		!reflect.DeepEqual(pending.PendingAssetIDs, []string{"asset_pending"}) || len(progress) == 0 {
		t.Fatalf("pending barrier=%#v progress=%#v", pending, progress)
	}

	if _, err := database.Write().ExecContext(t.Context(), `
		UPDATE jobs SET status='failed',error_json='{"message":"fixture VLM failed"}'
		WHERE asset_id='asset_pending'`); err != nil {
		t.Fatal(err)
	}
	failedRaw, err := exec.ExecuteTool(ctx, "shot.search", rushestools.ShotSearchInput{})
	if err != nil {
		t.Fatal(err)
	}
	failed := failedRaw.(rushestools.ShotSearchResult)
	if failed.ErrorCode != string(rushestools.ErrCodeShotIndexFailed) ||
		len(failed.FailedAssetIDs) != 1 || len(failed.Shots) != 0 {
		t.Fatalf("failed barrier=%#v", failed)
	}

	seedSearchIndex(t, database, "asset_pending", "hash_pending", "snapshot_pending", 1, "b_roll", []searchShotFixture{{
		id: "shot_pending", startFrame: 0, endFrame: 60, description: "补充产品镜头",
		tags: []string{"产品"}, subjects: []string{"产品"}, actions: []string{"展示"},
		setting: []string{"室外"}, shotScale: "远景", composition: "居中", quality: map[string]any{"label": "usable"},
	}})
	readyRaw, err := exec.ExecuteTool(ctx, "shot.search", rushestools.ShotSearchInput{Query: "产品", TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	ready := readyRaw.(rushestools.ShotSearchResult)
	if !ready.SearchReady || ready.Status != string(rushestools.StatusSucceeded) ||
		len(ready.Shots) != 2 || len(ready.PendingAssetIDs) != 0 || len(ready.FailedAssetIDs) != 0 {
		t.Fatalf("ready search=%#v", ready)
	}
}

func TestShotSearchFreezesAssetsBeforeWaitingAndStreamsCompletion(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_search_freeze"
	agenttest.CreateAgentDraft(t, database, draftID)
	seedSearchAsset(t, database, draftID, "asset_a", "hash_a", "a.mp4", "Broll")
	seedSearchAsset(t, database, draftID, "asset_b", "hash_b", "b.mp4", "Broll")
	seedSearchIndex(t, database, "asset_a", "hash_a", "snapshot_a", 1, "b_roll", []searchShotFixture{{
		id: "shot_a", startFrame: 0, endFrame: 60, description: "海边远景",
		tags: []string{"海边"}, subjects: []string{"海浪"}, actions: []string{"涌动"},
		setting: []string{"海岸"}, shotScale: "远景", composition: "水平", quality: map[string]any{"label": "usable"},
	}})
	seedBaseIndexJob(t, database, "asset_b", "hash_b", "pending")

	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	exec.jobPollInterval = 2 * time.Millisecond
	exec.jobWaitTimeout = time.Second
	pendingObserved := make(chan struct{})
	var once sync.Once
	progress := []map[string]any{}
	exec.progress = func(_ string, event map[string]any) {
		progress = append(progress, event)
		if completed, _ := event["completed"].(int); completed == 1 {
			once.Do(func() { close(pendingObserved) })
		}
	}
	writerDone := make(chan error, 1)
	go func() {
		<-pendingObserved
		if err := insertSearchAsset(database, draftID, "asset_late", "hash_late", "late.mp4", "Broll"); err != nil {
			writerDone <- err
			return
		}
		if err := insertSearchIndex(database, "asset_late", "hash_late", "snapshot_late", 1, "b_roll", []searchShotFixture{{
			id: "shot_late", startFrame: 0, endFrame: 60, description: "海边迟到素材",
			tags: []string{"海边"}, subjects: []string{"海浪"}, actions: []string{"涌动"},
			setting: []string{"海边"}, shotScale: "远景", composition: "水平", quality: map[string]any{"label": "usable"},
		}}); err != nil {
			writerDone <- err
			return
		}
		if err := insertSearchIndex(database, "asset_b", "hash_b", "snapshot_b", 1, "b_roll", []searchShotFixture{{
			id: "shot_b", startFrame: 0, endFrame: 60, description: "海滩环境镜头",
			tags: []string{"海滩"}, subjects: []string{"海浪"}, actions: []string{"涌动"},
			setting: []string{"海滩"}, shotScale: "全景", composition: "水平", quality: map[string]any{"label": "usable"},
		}}); err != nil {
			writerDone <- err
			return
		}
		writerDone <- nil
	}()

	raw, err := exec.ExecuteTool(
		rushestools.WithDraftID(t.Context(), draftID), "shot.search",
		rushestools.ShotSearchInput{Query: "海边远景", TopK: 5},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ShotSearchResult)
	if !reflect.DeepEqual(result.FrozenAssetIDs, []string{"asset_a", "asset_b"}) ||
		containsString(result.FrozenAssetIDs, "asset_late") || len(result.Shots) != 2 || len(progress) < 2 {
		t.Fatalf("frozen search=%#v progress=%#v", result, progress)
	}
	nextRaw, err := exec.ExecuteTool(
		rushestools.WithDraftID(t.Context(), draftID), "shot.search",
		rushestools.ShotSearchInput{Query: "海边远景", TopK: 5},
	)
	if err != nil {
		t.Fatal(err)
	}
	next := nextRaw.(rushestools.ShotSearchResult)
	if !containsString(next.FrozenAssetIDs, "asset_late") || next.IndexSnapshotID == result.IndexSnapshotID {
		t.Fatalf("下一次搜索应进入包含新素材的新快照: first=%#v next=%#v", result, next)
	}
}

func TestShotSearchWeightsExactFieldsSynonymsNegationAndDiversity(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_weighted_search"
	agenttest.CreateAgentDraft(t, database, draftID)
	fixtures := []struct {
		assetID, hash, filename string
		shots                   []searchShotFixture
	}{
		{
			assetID: "asset_exact", hash: "hash_exact", filename: "普通素材.mp4",
			shots: []searchShotFixture{{
				id: "shot_exact", startFrame: 0, endFrame: 60, description: "人物在海边舞蹈",
				tags: []string{"落日"}, subjects: []string{"人物"}, actions: []string{"舞蹈"},
				setting: []string{"海边"}, shotScale: "远景", composition: "居中",
				lighting: []string{"夕阳"}, quality: map[string]any{"label": "usable"},
			}},
		},
		{
			assetID: "asset_synonym", hash: "hash_synonym", filename: "沙滩黄昏.mp4",
			shots: []searchShotFixture{{
				id: "shot_synonym", startFrame: 0, endFrame: 60, description: "舞者在沙滩跳舞",
				tags: []string{"黄昏"}, subjects: []string{"舞者"}, actions: []string{"跳舞"},
				setting: []string{"海滩"}, shotScale: "全景", composition: "居中",
				lighting: []string{"晚霞"}, quality: map[string]any{"label": "usable"},
			}},
		},
		{
			assetID: "asset_filename_only", hash: "hash_filename", filename: "海边落日人物舞蹈远景.mp4",
			shots: []searchShotFixture{{
				id: "shot_filename", startFrame: 0, endFrame: 60, description: "室内静物",
				tags: []string{"产品"}, subjects: []string{"杯子"}, actions: []string{"静止"},
				setting: []string{"室内"}, shotScale: "特写", composition: "居中",
				quality: map[string]any{"label": "usable"},
			}},
		},
		{
			assetID: "asset_backlit", hash: "hash_backlit", filename: "键盘.mp4",
			shots: []searchShotFixture{{
				id: "shot_backlit", startFrame: 0, endFrame: 60, description: "键盘背光灯明亮开启",
				tags: []string{"背光"}, subjects: []string{"键盘"}, actions: []string{"灯光亮起"},
				setting: []string{"桌面"}, shotScale: "特写", composition: "俯拍",
				lighting: []string{"背光"}, quality: map[string]any{"label": "usable"},
			}},
		},
		{
			assetID: "asset_dark", hash: "hash_dark", filename: "暗键盘.mp4",
			shots: []searchShotFixture{{
				id: "shot_dark", startFrame: 0, endFrame: 60, description: "键盘不亮，环境全黑",
				tags: []string{"全黑"}, subjects: []string{"键盘"}, actions: []string{"静止"},
				setting: []string{"桌面"}, shotScale: "特写", composition: "俯拍",
				lighting: []string{"极低照度", "暗光"}, quality: map[string]any{"label": "dark"},
			}},
		},
	}
	for _, fixture := range fixtures {
		seedSearchAsset(t, database, draftID, fixture.assetID, fixture.hash, fixture.filename, "Broll")
		seedSearchIndex(t, database, fixture.assetID, fixture.hash, "snapshot_"+fixture.assetID, 1, "b_roll", fixture.shots)
	}
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)

	semanticRaw, err := exec.ExecuteTool(ctx, "shot.search", rushestools.ShotSearchInput{
		Query: "海边落日人物舞蹈远景", TopK: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	semantic := semanticRaw.(rushestools.ShotSearchResult)
	if len(semantic.Shots) < 2 || semantic.Shots[0].ShotID != "shot_exact" ||
		semantic.Shots[1].ShotID != "shot_synonym" || semantic.Shots[0].Score <= semantic.Shots[1].Score {
		t.Fatalf("字段权重/精确词/近义词排序=%#v", semantic.Shots)
	}
	if len(semantic.Shots) == 3 && semantic.Shots[2].ShotID == "shot_filename" &&
		semantic.Shots[2].Score >= semantic.Shots[1].Score {
		t.Fatalf("文件名权重不应高于结构化字段: %#v", semantic.Shots)
	}

	negativeRaw, err := exec.ExecuteTool(ctx, "shot.search", rushestools.ShotSearchInput{
		Query: "无背光键盘", TopK: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	negative := negativeRaw.(rushestools.ShotSearchResult)
	if len(negative.Shots) == 0 || negative.Shots[0].ShotID != "shot_dark" {
		t.Fatalf("否定语义未选中无背光镜头: %#v", negative.Shots)
	}
	for _, shot := range negative.Shots {
		if shot.ShotID == "shot_backlit" {
			t.Fatalf("无背光查询错误命中背光开启镜头: %#v", negative.Shots)
		}
	}

	topRaw, err := exec.ExecuteTool(ctx, "shot.search", rushestools.ShotSearchInput{TopK: 1})
	if err != nil {
		t.Fatal(err)
	}
	top := topRaw.(rushestools.ShotSearchResult)
	if len(top.Shots) != 1 || top.ReturnedCandidates != 1 || !top.Truncated {
		t.Fatalf("top_k=1 result=%#v", top)
	}
	if _, err := exec.ExecuteTool(ctx, "shot.search", rushestools.ShotSearchInput{TopK: 31}); err == nil {
		t.Fatal("top_k 超过 30 应校验失败")
	}
}

func TestShotSearchFiltersRoleTagsDurationQualityAndUsedRanges(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_search_filters"
	agenttest.CreateAgentDraft(t, database, draftID)
	seedSearchAsset(t, database, draftID, "asset_filter", "hash_filter", "filter.mp4", "Broll")
	seedSearchIndex(t, database, "asset_filter", "hash_filter", "snapshot_filter", 1, "b_roll", []searchShotFixture{
		{
			id: "shot_used", startFrame: 0, endFrame: 60, description: "海边人物奔跑",
			tags: []string{"海边", "奔跑"}, subjects: []string{"人物"}, actions: []string{"奔跑"},
			setting: []string{"海边"}, shotScale: "远景", composition: "跟拍", quality: map[string]any{"label": "usable"},
		},
		{
			id: "shot_available", startFrame: 60, endFrame: 150, description: "海边人物奔跑",
			tags: []string{"海边", "奔跑"}, subjects: []string{"人物"}, actions: []string{"奔跑"},
			setting: []string{"海边"}, shotScale: "远景", composition: "跟拍", quality: map[string]any{"label": "usable"},
		},
		{
			id: "shot_unusable", startFrame: 150, endFrame: 240, description: "海边人物奔跑",
			tags: []string{"海边", "奔跑"}, subjects: []string{"人物"}, actions: []string{"奔跑"},
			setting: []string{"海边"}, shotScale: "远景", composition: "跟拍", quality: map[string]any{"label": "unusable"},
		},
	})
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "asset_filter", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 60,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if persisted, err := seedTimelineVersion(exec, t.Context(), draftID, document, "search_filter_fixture", nil); err != nil || persisted.Status != "succeeded" {
		t.Fatalf("persisted=%#v err=%v", persisted, err)
	}
	raw, err := exec.ExecuteTool(
		rushestools.WithDraftID(t.Context(), draftID), "shot.search",
		rushestools.ShotSearchInput{
			Query: "海边奔跑", Tags: []string{"奔跑"}, SemanticRoles: []string{"b_roll"},
			MinDurationFrames: 61, MaxDurationFrames: 100, ExcludeUsed: true, TopK: 5,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ShotSearchResult)
	if len(result.Shots) != 1 || result.Shots[0].ShotID != "shot_available" {
		t.Fatalf("filtered result=%#v", result)
	}
	if _, err := exec.ExecuteTool(
		rushestools.WithDraftID(t.Context(), draftID), "shot.search",
		rushestools.ShotSearchInput{SemanticRoles: []string{"supporting"}},
	); err == nil {
		t.Fatal("未知 semantic role 应失败")
	}
	if _, err := exec.ExecuteTool(
		rushestools.WithDraftID(t.Context(), draftID), "shot.search",
		rushestools.ShotSearchInput{AssetIDs: []string{"missing"}},
	); err == nil {
		t.Fatal("未知 asset_id 应失败")
	}
}

func seedSearchAsset(
	t *testing.T,
	database *storage.DB,
	draftID, assetID, hash, filename, relDir string,
) {
	t.Helper()
	if err := insertSearchAsset(database, draftID, assetID, hash, filename, relDir); err != nil {
		t.Fatal(err)
	}
}

func insertSearchAsset(
	database *storage.DB,
	draftID, assetID, hash, filename, relDir string,
) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Write().Exec(`
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES(?, 'reference', ?, 'video', 'local_path', ?, ?, 1,
			'{"duration_sec":10}', 'ready', 'ready', 1)`,
		assetID, "/tmp/"+filename, filename, hash,
	); err != nil {
		return err
	}
	_, err := database.Write().Exec(`
		INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
		VALUES(?, ?, ?, ?)`, draftID, assetID, relDir, now)
	return err
}

func seedSearchIndex(
	t *testing.T,
	database *storage.DB,
	assetID, hash, snapshotID string,
	generation int,
	semanticRole string,
	shots []searchShotFixture,
) {
	t.Helper()
	if err := insertSearchIndex(database, assetID, hash, snapshotID, generation, semanticRole, shots); err != nil {
		t.Fatal(err)
	}
}

func insertSearchIndex(
	database *storage.DB,
	assetID, hash, snapshotID string,
	generation int,
	semanticRole string,
	shots []searchShotFixture,
) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	summaryJSON, _ := json.Marshal(map[string]any{"semantic_role": semanticRole})
	if _, err := database.Write().Exec(`
		INSERT INTO shot_index_snapshots(
			index_snapshot_id,asset_content_hash,generation,analyzer_version,
			output_schema_version,source_asset_id,status,summary_json,created_at,published_at
		) VALUES(?,?,?,'fixture-shot-index-v1',1,?,'ready',?,?,?)`,
		snapshotID, hash, generation, assetID, string(summaryJSON), now, now,
	); err != nil {
		return err
	}
	for _, shot := range shots {
		arrays := func(values []string) string {
			encoded, _ := json.Marshal(values)
			return string(encoded)
		}
		quality := shot.quality
		if quality == nil {
			quality = map[string]any{"label": "usable"}
		}
		qualityJSON, _ := json.Marshal(quality)
		searchValues := []string{shot.description}
		searchValues = append(searchValues, shot.tags...)
		searchValues = append(searchValues, shot.subjects...)
		searchValues = append(searchValues, shot.actions...)
		searchValues = append(searchValues, shot.setting...)
		searchValues = append(searchValues, shot.shotScale, shot.composition)
		searchText := strings.Join(searchValues, " ")
		searchTokensJSON, _ := json.Marshal(strings.Fields(searchText))
		framesJSON := fmt.Sprintf(`[{"source_frame":%d,"object_hash":"%s","object_size":1}]`,
			(shot.startFrame+shot.endFrame)/2, strings.Repeat("a", 64))
		if _, err := database.Write().Exec(`
			INSERT INTO shots(
				index_snapshot_id,shot_id,asset_content_hash,source_start_frame,source_end_frame,
				boundary_version,boundary_kind,boundary_confidence,lineage_parent_shot_id,
				representative_frames_json,description,tags_json,subjects_json,actions_json,
				setting_json,shot_scale,composition,lighting_json,mood_json,edit_hints_json,
				quality_json,search_text,search_tokens_json,deep_coverage_json,created_at
			) VALUES(?,?,?,?,?,1,'visual_cut',?,NULL,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			snapshotID, shot.id, hash, shot.startFrame, shot.endFrame, shot.boundaryConfidence,
			framesJSON, shot.description, arrays(shot.tags), arrays(shot.subjects), arrays(shot.actions),
			arrays(shot.setting), shot.shotScale, shot.composition, arrays(shot.lighting), arrays(shot.mood),
			arrays(shot.editHints), string(qualityJSON), searchText, string(searchTokensJSON),
			arrays(shot.deepCoverage), now,
		); err != nil {
			return err
		}
	}
	return nil
}

func seedBaseIndexJob(t *testing.T, database *storage.DB, assetID, hash, status string) {
	t.Helper()
	asset, err := storage.GetAsset(t.Context(), database.Read(), assetID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	idempotency := understandingBaseIndexKey(asset)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO jobs(
			job_id,kind,status,asset_id,idempotency_key,payload_json,max_retries,next_run_at,created_at
		) VALUES(?, 'understand', ?, ?, ?, '{}', 2, ?, ?)`,
		"job_"+hash, status, assetID, idempotency, now, now,
	); err != nil {
		t.Fatal(err)
	}
}

func understandingBaseIndexKey(asset storage.Asset) string {
	return "shot-base:" + baseIndexFixtureFingerprint(asset.Hash, asset.Kind)
}

func baseIndexFixtureFingerprint(hash, kind string) string {
	asset := storage.Asset{Hash: hash, Kind: kind}
	return understanding.BaseIndexFingerprint(asset)
}

func searchBusinessTruthCounts(t *testing.T, database *storage.DB) map[string]int {
	t.Helper()
	result := map[string]int{}
	for _, table := range []string{
		"assets", "shot_index_snapshots", "shots", "material_summaries", "jobs",
		"event_log", "timeline_versions",
	} {
		var count int
		if err := database.Read().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		result[table] = count
	}
	return result
}

func shotIDs(values []rushestools.ShotCandidate) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.AssetID+":"+value.ShotID)
	}
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
