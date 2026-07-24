package agentexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

const UnderstandJobEvidenceRuneBudget = 4000

type understandJobPayload struct {
	AssetID             string `json:"asset_id"`
	Focus               string `json:"focus"`
	Depth               string `json:"depth"`
	MaxStepsPerAsset    int    `json:"max_steps_per_asset"`
	ForceRefresh        bool   `json:"force_refresh"`
	RefreshNonce        string `json:"refresh_nonce"`
	AnalysisFingerprint string `json:"analysis_fingerprint"`
}

type understandJobEvidenceEnvelope struct {
	JobID string         `json:"job_id"`
	Asset map[string]any `json:"asset"`
}

func (exec *Executor) UnderstandJobEvidenceMessage(
	ctx context.Context,
	draftID string,
	jobID string,
) (*schema.Message, error) {
	var payloadJSON string
	var storedAssetID sql.NullString
	err := exec.database.Read().QueryRowContext(ctx, `
		SELECT payload_json, asset_id FROM jobs
		WHERE job_id=? AND kind='understand' AND status='succeeded'
		AND COALESCE(requested_by_draft_id, draft_id)=?`, jobID, draftID,
	).Scan(&payloadJSON, &storedAssetID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("understand job %s 不属于草稿 %s 或尚未成功", jobID, draftID)
	}
	if err != nil {
		return nil, err
	}
	payload, err := DecodeUnderstandJobPayload(payloadJSON)
	if err != nil {
		return nil, fmt.Errorf("understand job %s payload 无效: %w", jobID, err)
	}
	if !storedAssetID.Valid || strings.TrimSpace(storedAssetID.String) == "" {
		return nil, fmt.Errorf("understand job %s 缺少 asset_id", jobID)
	}
	assetID := strings.TrimSpace(storedAssetID.String)
	if payload.AssetID != assetID {
		return nil, fmt.Errorf(
			"understand job %s 的 payload asset_id=%s 与 job asset_id=%s 不一致",
			jobID, payload.AssetID, assetID,
		)
	}

	linkedAssets, err := storage.ListDraftAssets(ctx, exec.database.Read(), draftID)
	if err != nil {
		return nil, err
	}
	var asset storage.Asset
	linked := false
	for _, candidate := range linkedAssets {
		if candidate.ID == assetID {
			asset = candidate
			linked = true
			break
		}
	}
	if !linked {
		return nil, fmt.Errorf(
			"understand job %s 的素材 %s 已不再链接到草稿", jobID, assetID,
		)
	}
	raw, summaryErr := exec.materialSummaryForUnderstandJob(
		ctx, jobID, assetID, payload.AnalysisFingerprint,
	)
	if summaryErr != nil {
		return nil, fmt.Errorf(
			"understand job %s 已成功但素材 %s 缺少持久化摘要: %w",
			jobID, assetID, summaryErr,
		)
	}
	encodedSummary, _ := json.Marshal(raw)
	var summary understanding.Summary
	if err := json.Unmarshal(encodedSummary, &summary); err != nil {
		return nil, fmt.Errorf("素材 %s 的持久化摘要无效: %w", assetID, err)
	}
	item := map[string]any{
		"asset_id": asset.ID, "filename": TruncateRunes(asset.Filename, 160), "kind": asset.Kind,
		"overall":       TruncateRunes(strings.TrimSpace(summary.Overall), 256),
		"semantic_tags": limitedStrings(CatalogSemanticTags(summary.Segments, 10), 10, 64),
		"shot_count":    len(summary.Segments),
	}
	if summary.AnalysisDepth != "" {
		item["analysis_depth"] = summary.AnalysisDepth
	}
	if role := understanding.SuggestVisualRole(
		asset.Filename, valueOrEmpty(asset.RelDir), summary.SemanticRole,
	); role != "" {
		item["semantic_role"] = role
	}
	envelope := understandJobEvidenceEnvelope{JobID: jobID, Asset: item}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	header := "【本次后台素材理解结果（SQLite 持久化事实）】\n"
	content := header + string(encoded) +
		"\n逐镜头证据需要时再调用 shot.search；不要重复调用 media.detect_shots。"
	if len([]rune(content)) > UnderstandJobEvidenceRuneBudget {
		return nil, fmt.Errorf(
			"understand job %s 的证据超过 %d rune 预算",
			jobID, UnderstandJobEvidenceRuneBudget)
	}
	message := schema.SystemMessage(content)
	message.Extra = map[string]any{
		"context_phase": "job_understanding_evidence", "job_id": jobID,
	}
	return message, nil
}

func (exec *Executor) materialSummaryForUnderstandJob(
	ctx context.Context,
	jobID string,
	assetID string,
	fingerprint string,
) (map[string]any, error) {
	summaryID := fmt.Sprintf("summary_%s_%s", assetID, jobID)
	summary, err := storage.MaterialSummaryByID(ctx, exec.database.Read(), summaryID)
	if err == nil {
		return summary, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	if strings.TrimSpace(fingerprint) == "" {
		return nil, storage.ErrNotFound
	}
	return storage.MaterialSummaryByFingerprint(ctx, exec.database.Read(), assetID, fingerprint)
}

func DecodeUnderstandJobPayload(raw string) (understandJobPayload, error) {
	var payload understandJobPayload
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return understandJobPayload{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("包含多个 JSON 值")
		}
		return understandJobPayload{}, err
	}
	payload.AssetID = strings.TrimSpace(payload.AssetID)
	payload.AnalysisFingerprint = strings.TrimSpace(payload.AnalysisFingerprint)
	if payload.AssetID == "" {
		return understandJobPayload{}, errors.New("缺少非空 asset_id")
	}
	if payload.AnalysisFingerprint == "" {
		return understandJobPayload{}, errors.New("缺少非空 analysis_fingerprint")
	}
	return payload, nil
}

func limitedStrings(values []string, limit int, runeLimit int) []string {
	if len(values) > limit {
		values = values[:limit]
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, TruncateRunes(value, runeLimit))
	}
	return result
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
