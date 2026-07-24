package agentexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

const UnderstandJobEvidenceRuneBudget = 4000

type understandJobPayload struct {
	AssetIDs             []string
	Focus                string
	Depth                string
	MaxStepsPerAsset     int
	AnalysisFingerprints map[string]string
}

type understandJobEvidenceEnvelope struct {
	JobID            string           `json:"job_id"`
	Assets           []map[string]any `json:"assets"`
	Included         int              `json:"included"`
	Total            int              `json:"total"`
	Truncated        bool             `json:"truncated"`
	OmittedAssetIDs  []string         `json:"omitted_asset_ids,omitempty"`
	OmittedCount     int              `json:"omitted_count,omitempty"`
	UnlinkedAssetIDs []string         `json:"unlinked_asset_ids,omitempty"`
}

func (exec *Executor) UnderstandJobEvidenceMessage(
	ctx context.Context,
	draftID string,
	jobID string,
) (*schema.Message, error) {
	var payloadJSON string
	var legacyAssetID sql.NullString
	err := exec.database.Read().QueryRowContext(ctx, `
		SELECT payload_json, asset_id FROM jobs
		WHERE job_id=? AND kind='understand' AND status='succeeded'
		AND COALESCE(requested_by_draft_id, draft_id)=?`, jobID, draftID,
	).Scan(&payloadJSON, &legacyAssetID)
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
	if len(payload.AssetIDs) == 0 && legacyAssetID.Valid {
		payload.AssetIDs = []string{legacyAssetID.String}
	}
	assetIDs := deduplicateNonEmptyStrings(payload.AssetIDs)
	if len(assetIDs) == 0 {
		return nil, fmt.Errorf("understand job %s 缺少 asset_ids", jobID)
	}

	linkedAssets, err := storage.ListDraftAssets(ctx, exec.database.Read(), draftID)
	if err != nil {
		return nil, err
	}
	assetByID := make(map[string]storage.Asset, len(linkedAssets))
	for _, asset := range linkedAssets {
		assetByID[asset.ID] = asset
	}
	evidence := make([]map[string]any, 0, len(assetIDs))
	unlinked := make([]string, 0)
	for _, assetID := range assetIDs {
		asset, linked := assetByID[assetID]
		if !linked {
			unlinked = append(unlinked, assetID)
			continue
		}
		fingerprint := payload.AnalysisFingerprints[assetID]
		if fingerprint == "" {
			options := understanding.NormalizeAnalyzeOptions(asset, understanding.AnalyzeOptions{
				Focus: payload.Focus, Depth: payload.Depth, MaxStepsPerAsset: payload.MaxStepsPerAsset,
			})
			fingerprint = understanding.AnalysisFingerprint(asset, options)
		}
		raw, summaryErr := exec.materialSummaryForUnderstandJob(ctx, jobID, assetID, fingerprint)
		if summaryErr != nil {
			return nil, fmt.Errorf("understand job %s 已成功但素材 %s 缺少持久化摘要: %w",
				jobID, assetID, summaryErr)
		}
		encoded, _ := json.Marshal(raw)
		var summary understanding.Summary
		if err := json.Unmarshal(encoded, &summary); err != nil {
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
		if role := understanding.SuggestVisualRole(asset.Filename, valueOrEmpty(asset.RelDir), summary.SemanticRole); role != "" {
			item["semantic_role"] = role
		}
		evidence = append(evidence, item)
	}
	if len(evidence) == 0 {
		return nil, fmt.Errorf("understand job %s 没有仍链接到草稿的持久化摘要", jobID)
	}

	header := "【本次后台素材理解结果（SQLite 持久化事实）】\n"
	included := len(evidence)
	var content string
	for included > 0 {
		omitted := evidenceAssetIDs(evidence[included:])
		envelope := understandJobEvidenceEnvelope{
			JobID: jobID, Assets: evidence[:included], Included: included, Total: len(evidence),
			Truncated: included < len(evidence), OmittedAssetIDs: limitedStrings(omitted, 12, 128),
			OmittedCount: len(omitted), UnlinkedAssetIDs: limitedStrings(unlinked, 12, 128),
		}
		encoded, marshalErr := json.Marshal(envelope)
		if marshalErr != nil {
			return nil, marshalErr
		}
		content = header + string(encoded) + "\n逐镜头证据需要时再调用 shot.search；不要重复调用 media.detect_shots。"
		if len([]rune(content)) <= UnderstandJobEvidenceRuneBudget {
			break
		}
		included--
	}
	if included == 0 {
		return nil, fmt.Errorf("understand job %s 的最小证据仍超过 %d rune 预算",
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
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return understandJobPayload{}, err
	}
	payload := understandJobPayload{
		AssetIDs: looseStringSlice(value["asset_ids"]),
		Focus:    InterfaceString(value["focus"]), Depth: InterfaceString(value["depth"]),
		AnalysisFingerprints: map[string]string{},
	}
	if numeric, ok := NumericValue(value["max_steps_per_asset"]); ok {
		payload.MaxStepsPerAsset = int(numeric)
	}
	if fingerprints, ok := value["analysis_fingerprints"].(map[string]any); ok {
		for assetID, fingerprint := range fingerprints {
			if text, ok := fingerprint.(string); ok && strings.TrimSpace(text) != "" {
				payload.AnalysisFingerprints[assetID] = text
			}
		}
	}
	return payload, nil
}

func looseStringSlice(value any) []string {
	result := []string{}
	switch typed := value.(type) {
	case []string:
		return append(result, typed...)
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
	}
	return result
}

func deduplicateNonEmptyStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func evidenceAssetIDs(items []map[string]any) []string {
	result := make([]string, 0, len(items))
	for _, item := range items {
		if assetID, _ := item["asset_id"].(string); assetID != "" {
			result = append(result, assetID)
		}
	}
	return result
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
