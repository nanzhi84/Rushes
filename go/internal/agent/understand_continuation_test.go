package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

func TestUnderstandJobEvidenceSurvivesResidentCatalogTruncation(t *testing.T) {
	t.Parallel()
	const (
		draftID = "draft_understand_evidence_budget"
		assetID = "zzzz_target_understand_evidence"
	)
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, draftID)
	events := make([]contracts.Event, 0, 240)
	for index := range 80 {
		fillerID := fmt.Sprintf("asset_filler_%03d_%s", index, strings.Repeat("x", 72))
		events = append(events,
			contracts.Event{Type: "AssetImported", Payload: map[string]any{
				"asset_id": fillerID, "job_id": "import_" + fillerID,
				"kind": "font", "filename": strings.Repeat("长素材名", 36) + fmt.Sprint(index),
				"hash": "hash_" + fillerID, "ingest_status": "ready", "usable": true,
			}},
			contracts.Event{Type: "AssetLinked", DraftID: draftID, Payload: map[string]any{
				"asset_id": fillerID,
			}},
			contracts.Event{Type: "MaterialUnderstandingCompleted", Payload: map[string]any{
				"asset_id": fillerID,
			}},
		)
	}
	result, err := reducer.Apply(t.Context(), database, events, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("fillers result=%#v err=%v", result, err)
	}
	addUnderstandRoutingAsset(t, database, draftID, assetID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(exec.Close)
	input := rushestools.UnderstandInput{AssetIDs: []string{assetID}, Depth: "deep"}
	queued, err := exec.toolUnderstand(t.Context(), draftID, input)
	if err != nil {
		t.Fatal(err)
	}
	cacheUnderstandRoutingSummary(t, database, assetID, input)
	terminal, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobSucceeded", DraftID: draftID, Payload: map[string]any{
			"job_id": queued.JobID, "kind": "understand", "requested_by_draft_id": draftID,
			"result": map[string]any{"status": "completed"},
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || terminal.Status != reducer.StatusApplied {
		t.Fatalf("terminal=%#v err=%v", terminal, err)
	}

	snapshot, err := NewContextBuilder(database).Snapshot(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	rawSnapshot, err := snapshot.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawSnapshot), assetID) {
		t.Fatalf("目标素材必须先被 resident material_catalog 截断，snapshot=%s", rawSnapshot)
	}
	if !strings.Contains(string(rawSnapshot), `"material_catalog_truncated":true`) {
		t.Fatalf("fixture 未触发 material catalog 截断: %s", rawSnapshot)
	}

	message, err := exec.executor.UnderstandJobEvidenceMessage(t.Context(), draftID, queued.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if message.Extra["context_phase"] != "job_understanding_evidence" ||
		message.Extra["job_id"] != queued.JobID {
		t.Fatalf("evidence extra=%#v", message.Extra)
	}
	for _, expected := range []string{assetID, "缓存摘要 " + assetID, `"included":1`, `"truncated":false`} {
		if !strings.Contains(message.Content, expected) {
			t.Fatalf("evidence 缺少 %q: %s", expected, message.Content)
		}
	}
	if len([]rune(message.Content)) > understandJobEvidenceRuneBudget {
		t.Fatalf("evidence runes=%d budget=%d", len([]rune(message.Content)), understandJobEvidenceRuneBudget)
	}
	if strings.Contains(message.Content, `"segments"`) {
		t.Fatalf("定向证据不应常驻逐镜头 segments: %s", message.Content)
	}
}

func TestDecodeUnderstandJobPayloadKeepsLegacyMixedAssetIDs(t *testing.T) {
	t.Parallel()
	payload, err := decodeUnderstandJobPayload(`{
		"asset_ids":["asset-a",42,"asset-b",null],
		"focus":"人物","depth":"deep","max_steps_per_asset":9,
		"analysis_fingerprints":{"asset-a":"fp-a","asset-b":17}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(payload.AssetIDs, ","); got != "asset-a,asset-b" ||
		payload.Focus != "人物" || payload.Depth != "deep" || payload.MaxStepsPerAsset != 9 ||
		payload.AnalysisFingerprints["asset-a"] != "fp-a" {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestUnderstandJobEvidenceAndTerminalReuseSelectJobSpecificSummary(t *testing.T) {
	t.Parallel()
	const (
		draftID       = "draft_understand_job_specific"
		assetID       = "asset_understand_job_specific"
		oldMarker     = "OLD_GLOBAL_BEST_MARKER"
		currentMarker = "CURRENT_JOB_MARKER"
	)
	database, exec := setupUnderstandRoutingService(t, draftID, assetID)
	input := rushestools.UnderstandInput{
		AssetIDs: []string{assetID}, Focus: "只识别 logo 颜色", Depth: "deep",
	}
	queued, err := exec.toolUnderstand(t.Context(), draftID, input)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := storage.GetAsset(t.Context(), database.Read(), assetID)
	if err != nil {
		t.Fatal(err)
	}
	options := understanding.NormalizeAnalyzeOptions(asset, understanding.AnalyzeOptions{
		Focus: input.Focus, Depth: input.Depth, MaxStepsPerAsset: input.MaxStepsPerAsset,
	})
	fingerprint := understanding.AnalysisFingerprint(asset, options)
	oldSegments := make([]map[string]any, 0, 6)
	for index := range 6 {
		oldSegments = append(oldSegments, map[string]any{
			"start_s": index, "end_s": index + 1, "description": "旧的丰富通用证据",
			"tags": []string{"旧标签"}, "subjects": []string{"旧主体"}, "actions": []string{"旧动作"},
		})
	}
	longTags := make([]string, 0, 10)
	for index := range 10 {
		longTags = append(longTags, fmt.Sprintf("tag-%d-%s", index, strings.Repeat("超长", 600)))
	}
	oldFingerprint := "old-global-fingerprint"
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorJob,
		ResultRows: reducer.ResultRows{MaterialSummaries: []reducer.MaterialSummaryRow{
			{
				ID: "summary_old_global_best", AssetID: assetID, Status: "ready",
				Fingerprint: &oldFingerprint, Summary: map[string]any{
					"asset_id": assetID, "overall": oldMarker, "semantic_role": "visual",
					"segments": oldSegments, "analysis_depth": "deep", "model": "fixture-old",
				},
			},
			{
				ID: "summary_" + assetID + "_" + queued.JobID, AssetID: assetID, Status: "ready",
				Fingerprint: &fingerprint, Summary: map[string]any{
					"asset_id": assetID, "overall": currentMarker, "semantic_role": "visual",
					"segments": []map[string]any{{
						"start_s": 0, "end_s": 1, "description": "本 job 的聚焦结果", "tags": longTags,
					}},
					"analysis_depth": "deep", "model": "fixture-current",
				},
			},
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("summaries result=%#v err=%v", result, err)
	}
	best, err := storage.BestMaterialSummary(t.Context(), database.Read(), assetID)
	if err != nil || best["overall"] != oldMarker {
		t.Fatalf("fixture 必须让全局 Best 选旧摘要: best=%#v err=%v", best, err)
	}
	terminal, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobSucceeded", DraftID: draftID, Payload: map[string]any{
			"job_id": queued.JobID, "kind": "understand", "requested_by_draft_id": draftID,
			"result": map[string]any{"status": "completed", "analyzed_asset_ids": []string{assetID}},
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || terminal.Status != reducer.StatusApplied {
		t.Fatalf("terminal=%#v err=%v", terminal, err)
	}

	message, err := exec.executor.UnderstandJobEvidenceMessage(t.Context(), draftID, queued.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message.Content, currentMarker) || strings.Contains(message.Content, oldMarker) {
		t.Fatalf("定向证据未绑定当前 job: %s", message.Content)
	}
	if len([]rune(message.Content)) > understandJobEvidenceRuneBudget {
		t.Fatalf("长 tag 后 evidence runes=%d budget=%d",
			len([]rune(message.Content)), understandJobEvidenceRuneBudget)
	}
	repeated, err := exec.toolUnderstand(t.Context(), draftID, input)
	if err != nil || repeated.JobID != queued.JobID || repeated.Status != "completed" ||
		len(repeated.Summaries) != 1 || repeated.Summaries[0].Overall != currentMarker {
		t.Fatalf("terminal reuse=%#v err=%v", repeated, err)
	}
}
