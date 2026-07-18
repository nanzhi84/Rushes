package agentexec

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestContentContractSchemaValidationAndDeterministicVerification(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_contract")
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,size,probe_json,ingest_status,understanding_status,usable)
		VALUES
		('asset_a','reference','/tmp/a.mp4','video','local_path','a.mp4','hash_a',1,'{}','ready','done',1),
		('asset_b','reference','/tmp/b.mp4','video','local_path','b.mp4','hash_b',1,'{}','ready','done',1);
		INSERT INTO transcripts(transcript_id,asset_id,provider_id,raw_preserved,utterances_json,vad_segments_json)
		VALUES('transcript_a','asset_a','fixture',0,
			'[{"utterance_id":"utt_keep","source_start_frame":0,"source_end_frame":30,"text":"必须保留"}]','[]')
	`); err != nil {
		t.Fatal(err)
	}
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report, configured, err := exec.VerifyContentContract(t.Context(), "draft_contract", timeline.Empty("draft_contract", 1)); err != nil || configured || len(report.Items) != 0 {
		t.Fatalf("empty contract report=%#v configured=%v err=%v", report, configured, err)
	}
	invalidRatio := 1.1
	invalid := rushestools.PlanUpdateInput{Plan: map[string]any{}, Contract: &rushestools.ContentPlanContract{
		MinOnBeatRatio: &invalidRatio,
	}}
	result, err := exec.toolPlanUpdate(t.Context(), "draft_contract", invalid)
	if err != nil || result.Status != "failed" || !strings.Contains(result.Observation, "0 到 1") {
		t.Fatalf("invalid result=%#v err=%v", result, err)
	}
	minRatio := 0.9
	tolerance := 2
	minDensity, maxDensity := 10.0, 19.0
	result, err = exec.toolPlanUpdate(t.Context(), "draft_contract", rushestools.PlanUpdateInput{
		Plan: map[string]any{"goal": "合同测试"},
		Contract: &rushestools.ContentPlanContract{
			TargetDurationFrames: 120, DurationToleranceFrames: &tolerance,
			MustKeepUtteranceIDs: []string{"utt_keep"},
			BrollCoverageRanges:  []rushestools.ContentPlanFrameRange{{StartFrame: 10, EndFrame: 30}},
			MinOnBeatRatio:       &minRatio, Rhythm: "紧凑",
			MinCutDensityPerMinute: &minDensity, MaxCutDensityPerMinute: &maxDensity,
		},
	})
	if err != nil || result.Status != "succeeded" {
		t.Fatalf("plan result=%#v err=%v", result, err)
	}
	draft, err := storage.GetDraft(t.Context(), database.Read(), "draft_contract")
	if err != nil || draft.ContentPlan["contract"] == nil {
		t.Fatalf("draft=%#v err=%v", draft, err)
	}

	failing, err := timeline.ComposeInitial("draft_contract", 1, []timeline.Selection{
		{AssetID: "asset_a", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 10, Role: "a_roll"},
		{AssetID: "asset_b", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 80, Role: "a_roll"},
	})
	if err != nil {
		t.Fatal(err)
	}
	failing.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "bgm_contract", TrackID: "bgm", AssetID: "bgm",
		TimelineStartFrame: 0, TimelineEndFrame: 90, SourceStartFrame: 0, SourceEndFrame: 90,
		Effects: []map[string]any{{"kind": "beat_grid", "beat_frames": []int{30, 60}}},
	}}
	report, configured, err := exec.VerifyContentContract(t.Context(), "draft_contract", failing)
	if err != nil || !configured || report.Pass || len(ContractFailureItems(report)) != 5 {
		t.Fatalf("failing report=%#v configured=%v err=%v", report, configured, err)
	}
	byCheck := map[string]ContractVerificationItem{}
	for _, item := range report.Items {
		byCheck[item.Check] = item
	}
	if len(byCheck["target_duration"].Frames) != 2 ||
		len(byCheck["must_keep_utterances"].IDs) != 1 || len(byCheck["must_keep_utterances"].Frames) != 2 ||
		len(byCheck["on_beat_ratio"].Frames) != 1 {
		t.Fatalf("missing anchors report=%#v", report)
	}
	persisted, err := exec.persistTimeline(t.Context(), "draft_contract", failing, "contract_fixture")
	if err != nil || !strings.Contains(persisted.Observation, "验收合同未通过项") || persisted.Data["contract_failures"] == nil {
		t.Fatalf("persisted=%#v err=%v", persisted, err)
	}
	assertPersistedContractReport(t, database, "draft_contract", 1, false)
	validated, err := exec.toolValidateTimeline(t.Context(), "draft_contract")
	if err != nil || validated.Status != "succeeded" ||
		!strings.Contains(validated.Observation, "验收合同未通过项") ||
		len(validated.Data["contract_failures"].([]ContractVerificationItem)) != 5 ||
		validated.Data["content_contract"].(ContractVerificationReport).Pass {
		t.Fatalf("validated failing contract=%#v err=%v", validated, err)
	}

	compliant, err := timeline.ComposeInitial("draft_contract", 2, []timeline.Selection{
		{AssetID: "asset_a", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 30, Role: "a_roll"},
		{AssetID: "asset_b", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 90, Role: "a_roll"},
	})
	if err != nil {
		t.Fatal(err)
	}
	compliant.Tracks[1].Clips = []timeline.Clip{{
		TimelineClipID: "overlay_contract", TrackID: "visual_overlay", AssetID: "asset_b",
		TimelineStartFrame: 10, TimelineEndFrame: 30, SourceStartFrame: 0, SourceEndFrame: 20,
	}}
	compliant.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "bgm_contract_ok", TrackID: "bgm", AssetID: "bgm",
		TimelineStartFrame: 0, TimelineEndFrame: 120, SourceStartFrame: 0, SourceEndFrame: 120,
		Effects: []map[string]any{{"kind": "beat_grid", "beat_frames": []int{30}}},
	}}
	report, configured, err = exec.VerifyContentContract(t.Context(), "draft_contract", compliant)
	if err != nil || !configured || !report.Pass || len(ContractFailureItems(report)) != 0 {
		t.Fatalf("compliant report=%#v configured=%v err=%v", report, configured, err)
	}
	persisted, err = exec.persistTimeline(t.Context(), "draft_contract", compliant, "contract_compliant_fixture")
	if err != nil {
		t.Fatal(err)
	}
	assertPersistedContractReport(t, database, "draft_contract", 2, true)
	validated, err = exec.toolValidateTimeline(t.Context(), "draft_contract")
	if err != nil || validated.Status != "succeeded" ||
		!strings.Contains(validated.Observation, "验收合同全部通过") ||
		len(validated.Data["contract_failures"].([]ContractVerificationItem)) != 0 ||
		!validated.Data["content_contract"].(ContractVerificationReport).Pass {
		t.Fatalf("validated passing contract=%#v err=%v", validated, err)
	}
	for index := range compliant.Tracks {
		if compliant.Tracks[index].TrackID == "original_audio" {
			compliant.Tracks[index].Clips = []timeline.Clip{{
				TrackID: "original_audio", AssetID: "asset_a",
				SourceStartFrame: 0, SourceEndFrame: 10,
				TimelineStartFrame: 0, TimelineEndFrame: 10,
			}}
		}
	}
	missing, anchors, err := exec.missingRequiredUtterances(t.Context(), compliant, []string{"utt_keep"})
	if err != nil || len(missing) != 1 || missing[0] != "utt_keep" || len(anchors) != 2 {
		t.Fatalf("explicit audio must drive speech contract: missing=%#v anchors=%#v err=%v", missing, anchors, err)
	}
}

func TestContentContractReportsMissingBeatGrid(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_missing_beat_grid")
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}

	minRatio := 0.8
	updated, err := exec.toolPlanUpdate(t.Context(), "draft_missing_beat_grid", rushestools.PlanUpdateInput{
		Plan:     map[string]any{"goal": "缺节拍网格合同测试"},
		Contract: &rushestools.ContentPlanContract{MinOnBeatRatio: &minRatio},
	})
	if err != nil || updated.Status != "succeeded" {
		t.Fatalf("plan update=%#v err=%v", updated, err)
	}
	document := timeline.Empty("draft_missing_beat_grid", 1)
	report, configured, err := exec.VerifyContentContract(t.Context(), "draft_missing_beat_grid", document)
	if err != nil || !configured || report.Pass || len(report.Items) != 1 {
		t.Fatalf("report=%#v configured=%v err=%v", report, configured, err)
	}
	item := report.Items[0]
	const guidance = "无法核对卡拍比例：当前 BGM 无节拍网格，请先 audio.analyze_beats 或用 recut_to_beats 重建"
	if item.Check != "on_beat_ratio" || item.Pass || item.ErrorCode != "missing_beat_grid" || item.Message != guidance {
		t.Fatalf("item=%#v", item)
	}
	persisted, err := exec.persistTimeline(t.Context(), "draft_missing_beat_grid", document, "missing_beat_grid_fixture")
	if err != nil || !strings.Contains(persisted.Observation, guidance) {
		t.Fatalf("persisted=%#v err=%v", persisted, err)
	}
}

func assertPersistedContractReport(t *testing.T, database *storage.DB, draftID string, version int, wantPass bool) {
	t.Helper()
	var raw string
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT validation_report_json FROM timeline_versions WHERE draft_id=? AND version=?`,
		draftID, version,
	).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	report := struct {
		ContentContract ContractVerificationReport `json:"content_contract"`
	}{}
	if err := json.Unmarshal([]byte(raw), &report); err != nil || report.ContentContract.Pass != wantPass || len(report.ContentContract.Items) == 0 {
		t.Fatalf("validation_report_json=%s report=%#v err=%v", raw, report, err)
	}
}

func TestContentContractRejectsUnknownFieldsWithoutRestrictingPlan(t *testing.T) {
	plan := map[string]any{
		"free_form_extension": map[string]any{"anything": true},
		"contract":            map[string]any{"target_duration_frame": 120},
	}
	if _, err := ContentPlanContract(plan); err == nil {
		t.Fatal("拼错的合同字段必须被拒绝")
	}
	plan["contract"] = map[string]any{"target_duration_frames": 120}
	if _, err := ContentPlanContract(plan); err != nil {
		t.Fatalf("合同外的 plan 自由字段不应受限: %v", err)
	}
}

func TestContentContractDistinguishesOmittedAndExplicitZeroTolerance(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_tolerance")
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document := timeline.Empty("draft_tolerance", 1)
	document.FPS = 30
	document.DurationFrames = 130

	verify := func(contract map[string]any) ContractVerificationReport {
		draft, getErr := storage.GetDraft(t.Context(), database.Read(), "draft_tolerance")
		if getErr != nil {
			t.Fatal(getErr)
		}
		draft.ContentPlan = map[string]any{"contract": contract}
		encoded, marshalErr := json.Marshal(draft.ContentPlan)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if _, updateErr := database.Write().ExecContext(t.Context(), `UPDATE drafts SET content_plan_json=? WHERE draft_id=?`, string(encoded), draft.ID); updateErr != nil {
			t.Fatal(updateErr)
		}
		report, configured, verifyErr := exec.VerifyContentContract(t.Context(), draft.ID, document)
		if verifyErr != nil || !configured {
			t.Fatalf("report=%#v configured=%v err=%v", report, configured, verifyErr)
		}
		return report
	}

	if report := verify(map[string]any{"target_duration_frames": 120}); !report.Pass {
		t.Fatalf("省略容差应使用 15 帧默认值: %#v", report)
	}
	if report := verify(map[string]any{"target_duration_frames": 120, "duration_tolerance_frames": 0}); report.Pass {
		t.Fatalf("显式零容差必须精确匹配: %#v", report)
	}
	if report := verify(map[string]any{"target_duration_frames": 120, "duration_tolerance_frames": 10}); !report.Pass {
		t.Fatalf("显式正容差应按给定值验收: %#v", report)
	}
}

func TestMissingRequiredUtterancesAggregatesDuplicateIDsAcrossAssetsDeterministically(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_duplicate_utterance")
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(asset_id,storage_mode,reference_path,kind,source,filename,hash,size,probe_json,ingest_status,understanding_status,usable)
		VALUES
		('asset_a','reference','/tmp/a.mp4','video','local_path','a.mp4','hash_duplicate_a',1,'{}','ready','done',1),
		('asset_b','reference','/tmp/b.mp4','video','local_path','b.mp4','hash_duplicate_b',1,'{}','ready','done',1);
		INSERT INTO transcripts(transcript_id,asset_id,provider_id,raw_preserved,utterances_json,vad_segments_json)
		VALUES
		('transcript_duplicate_a','asset_a','fixture',0,'[{"utterance_id":"utt_1","source_start_frame":0,"source_end_frame":30,"text":"素材 A"}]','[]'),
		('transcript_duplicate_b','asset_b','fixture',0,'[{"utterance_id":"utt_1","source_start_frame":0,"source_end_frame":30,"text":"素材 B"}]','[]')
	`); err != nil {
		t.Fatal(err)
	}
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}

	document := timeline.Empty("draft_duplicate_utterance", 1)
	document.Tracks[0].Clips = []timeline.Clip{
		{AssetID: "asset_a", SourceStartFrame: 0, SourceEndFrame: 30, TimelineStartFrame: 0, TimelineEndFrame: 30},
		{AssetID: "asset_b", SourceStartFrame: 0, SourceEndFrame: 10, TimelineStartFrame: 30, TimelineEndFrame: 40},
	}
	for iteration := 0; iteration < 100; iteration++ {
		missing, anchors, verifyErr := exec.missingRequiredUtterances(t.Context(), document, []string{"utt_1"})
		if verifyErr != nil || len(missing) != 0 || len(anchors) != 0 {
			t.Fatalf("iteration=%d missing=%#v anchors=%#v err=%v", iteration, missing, anchors, verifyErr)
		}
	}

	document.Tracks[0].Clips[0].SourceEndFrame = 10
	document.Tracks[0].Clips[0].TimelineEndFrame = 10
	for iteration := 0; iteration < 100; iteration++ {
		missing, anchors, verifyErr := exec.missingRequiredUtterances(t.Context(), document, []string{"utt_1"})
		if verifyErr != nil || len(missing) != 1 || missing[0] != "utt_1" || len(anchors) != 2 || anchors[0] != 0 || anchors[1] != 30 {
			t.Fatalf("iteration=%d missing=%#v anchors=%#v err=%v", iteration, missing, anchors, verifyErr)
		}
	}
}

func TestContentContractRejectsInvalidRangesAndDensity(t *testing.T) {
	minDensity, maxDensity := 30.0, 20.0
	for _, plan := range []map[string]any{
		{"contract": map[string]any{"broll_coverage_ranges": []any{map[string]any{"start_frame": 20, "end_frame": 10}}}},
		{"contract": map[string]any{"min_cut_density_per_minute": minDensity, "max_cut_density_per_minute": maxDensity}},
	} {
		if _, err := ContentPlanContract(plan); err == nil {
			t.Fatalf("plan should fail: %#v", plan)
		}
	}
}

func TestContentContractNormalizesAndRejectsEmptyUtteranceIDs(t *testing.T) {
	for _, ids := range []any{[]any{}, []any{""}, []any{" "}, []any{"valid", ""}} {
		if _, err := ContentPlanContract(map[string]any{
			"contract": map[string]any{"must_keep_utterance_ids": ids},
		}); err == nil {
			t.Fatalf("empty utterance id must fail: %#v", ids)
		}
	}
	contract, err := ContentPlanContract(map[string]any{
		"contract": map[string]any{"must_keep_utterance_ids": []any{" utt_a ", "utt_a", "utt_b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ids, ok := contract["must_keep_utterance_ids"].([]any)
	if !ok || len(ids) != 2 || ids[0] != "utt_a" || ids[1] != "utt_b" {
		t.Fatalf("normalized contract=%#v", contract)
	}
}

func TestPlanUpdateTypedContractPreservesMustKeepUtteranceValidation(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_typed_contract")
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		payload    string
		wantStatus string
	}{
		{name: "omitted", payload: `{"plan":{},"contract":{}}`, wantStatus: "succeeded"},
		{name: "empty", payload: `{"plan":{},"contract":{"must_keep_utterance_ids":[]}}`, wantStatus: "failed"},
		{name: "blank", payload: `{"plan":{},"contract":{"must_keep_utterance_ids":[" "]}}`, wantStatus: "failed"},
		{name: "valid", payload: `{"plan":{},"contract":{"must_keep_utterance_ids":[" keep "]}}`, wantStatus: "succeeded"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var input rushestools.PlanUpdateInput
			if err := json.Unmarshal([]byte(test.payload), &input); err != nil {
				t.Fatal(err)
			}
			result, err := exec.toolPlanUpdate(t.Context(), "draft_typed_contract", input)
			if err != nil || result.Status != test.wantStatus {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			if test.wantStatus == "failed" && result.Data["reason"] != "contract_invalid" {
				t.Fatalf("failure=%#v", result)
			}
		})
	}

	draft, err := storage.GetDraft(t.Context(), database.Read(), "draft_typed_contract")
	contract, _ := draft.ContentPlan["contract"].(map[string]any)
	ids, _ := contract["must_keep_utterance_ids"].([]any)
	if err != nil || len(ids) != 1 || ids[0] != "keep" {
		t.Fatalf("contract=%#v err=%v", contract, err)
	}
}

func TestContentPreservingClipsHonorAudioSolo(t *testing.T) {
	document := timeline.Empty("solo_contract", 1)
	document.Tracks[0].Clips = []timeline.Clip{{
		TrackID: "visual_base", AssetID: "implicit", TimelineEndFrame: 30, SourceEndFrame: 30,
	}}
	document.Tracks[3].Clips = []timeline.Clip{{
		TrackID: "voiceover", AssetID: "voice", TimelineEndFrame: 30, SourceEndFrame: 30,
	}}

	document.Tracks[3].Solo = true
	if clips := ContentPreservingClips(document); len(clips) != 1 || clips[0].AssetID != "voice" {
		t.Fatalf("voiceover solo clips=%#v", clips)
	}
	document.Tracks[3].Solo = false
	document.Tracks[2].Solo = true
	if clips := ContentPreservingClips(document); len(clips) != 1 || clips[0].AssetID != "implicit" {
		t.Fatalf("implicit original solo clips=%#v", clips)
	}
	document.Tracks[2].Muted = true
	if clips := ContentPreservingClips(document); len(clips) != 1 || clips[0].AssetID != "voice" {
		t.Fatalf("muted solo must not suppress audible tracks: %#v", clips)
	}
	document.Tracks[4].Solo = true
	if clips := ContentPreservingClips(document); len(clips) != 0 {
		t.Fatalf("bgm solo must suppress speech clips: %#v", clips)
	}
}

func TestUtteranceCoverageAcceptsOnlyContinuousLosslessSplits(t *testing.T) {
	split := []timeline.Clip{
		{AssetID: "asset", SourceStartFrame: 0, SourceEndFrame: 15, TimelineStartFrame: 10, TimelineEndFrame: 25, PlaybackRate: 1},
		{AssetID: "asset", SourceStartFrame: 15, SourceEndFrame: 30, TimelineStartFrame: 25, TimelineEndFrame: 40, PlaybackRate: 1},
	}
	if !UtteranceCoveredByClips(split, "asset", 0, 30) {
		t.Fatal("source- and timeline-continuous split must preserve utterance")
	}
	sourceGap := append([]timeline.Clip(nil), split...)
	sourceGap[1].SourceStartFrame = 16
	if UtteranceCoveredByClips(sourceGap, "asset", 0, 30) {
		t.Fatal("source gap must fail")
	}
	timelineGap := append([]timeline.Clip(nil), split...)
	timelineGap[1].TimelineStartFrame = 26
	timelineGap[1].TimelineEndFrame = 41
	if UtteranceCoveredByClips(timelineGap, "asset", 0, 30) {
		t.Fatal("timeline gap must fail")
	}
	invalidRate := append([]timeline.Clip(nil), split...)
	invalidRate[1].PlaybackRate = 2
	if UtteranceCoveredByClips(invalidRate, "asset", 0, 30) {
		t.Fatal("clip whose timeline duration disagrees with playback rate must fail")
	}
	changedRate := []timeline.Clip{
		split[0],
		{AssetID: "asset", SourceStartFrame: 15, SourceEndFrame: 29, TimelineStartFrame: 25, TimelineEndFrame: 32, PlaybackRate: 2},
	}
	if UtteranceCoveredByClips(changedRate, "asset", 0, 29) {
		t.Fatal("split with a playback-rate discontinuity is not lossless")
	}
}
