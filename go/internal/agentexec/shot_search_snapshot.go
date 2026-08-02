package agentexec

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const shotSearchSnapshotPrefix = "shot_search_v1."

type shotSearchSnapshotAsset struct {
	AssetID        string `json:"asset_id"`
	ContentHash    string `json:"content_hash"`
	BaseSnapshotID string `json:"base_snapshot_id"`
	Generation     int    `json:"generation"`
}

type shotSearchSnapshot struct {
	DraftID        string                    `json:"draft_id"`
	SynonymVersion string                    `json:"synonym_version"`
	Assets         []shotSearchSnapshotAsset `json:"assets"`
}

// shotSearchSnapshotID is a self-verifying, read-only snapshot token. Keeping
// the frozen asset-to-base-index mapping in the token lets shot.deep_search
// validate a past ShotRef without making shot.search write a derived cache row.
func shotSearchSnapshotID(draftID string, values []frozenShotAsset) string {
	payload := shotSearchSnapshot{
		DraftID: strings.TrimSpace(draftID), SynonymVersion: shotSearchSynonymVersion,
		Assets: make([]shotSearchSnapshotAsset, 0, len(values)),
	}
	for _, value := range values {
		payload.Assets = append(payload.Assets, shotSearchSnapshotAsset{
			AssetID: value.asset.ID, ContentHash: value.asset.Hash,
			BaseSnapshotID: value.snapshot.ID, Generation: value.snapshot.Generation,
		})
	}
	sort.Slice(payload.Assets, func(left, right int) bool {
		return payload.Assets[left].AssetID < payload.Assets[right].AssetID
	})
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("编码 shot search snapshot: %v", err))
	}
	digest := sha256.Sum256(encoded)
	return shotSearchSnapshotPrefix + base64.RawURLEncoding.EncodeToString(encoded) + "." +
		hex.EncodeToString(digest[:16])
}

func parseShotSearchSnapshot(token string) (shotSearchSnapshot, error) {
	remainder, found := strings.CutPrefix(strings.TrimSpace(token), shotSearchSnapshotPrefix)
	if !found {
		return shotSearchSnapshot{}, errors.New("index_snapshot_id 不是当前版本的冻结搜索快照")
	}
	parts := strings.Split(remainder, ".")
	if len(parts) != 2 || parts[0] == "" || len(parts[1]) != 32 {
		return shotSearchSnapshot{}, errors.New("index_snapshot_id 结构无效")
	}
	encoded, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return shotSearchSnapshot{}, errors.New("index_snapshot_id payload 无法解码")
	}
	providedDigest, err := hex.DecodeString(parts[1])
	if err != nil {
		return shotSearchSnapshot{}, errors.New("index_snapshot_id checksum 无法解码")
	}
	digest := sha256.Sum256(encoded)
	if subtle.ConstantTimeCompare(providedDigest, digest[:16]) != 1 {
		return shotSearchSnapshot{}, errors.New("index_snapshot_id checksum 不匹配")
	}
	var payload shotSearchSnapshot
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return shotSearchSnapshot{}, errors.New("index_snapshot_id payload 不是有效 JSON")
	}
	if strings.TrimSpace(payload.DraftID) == "" ||
		payload.SynonymVersion != shotSearchSynonymVersion || len(payload.Assets) == 0 {
		return shotSearchSnapshot{}, errors.New("index_snapshot_id payload 字段不完整")
	}
	seen := map[string]struct{}{}
	previous := ""
	for _, asset := range payload.Assets {
		if strings.TrimSpace(asset.AssetID) == "" || strings.TrimSpace(asset.ContentHash) == "" ||
			strings.TrimSpace(asset.BaseSnapshotID) == "" || asset.Generation < 1 {
			return shotSearchSnapshot{}, errors.New("index_snapshot_id 含无效素材索引")
		}
		if _, duplicate := seen[asset.AssetID]; duplicate || previous != "" && asset.AssetID < previous {
			return shotSearchSnapshot{}, errors.New("index_snapshot_id 素材集合不唯一或未排序")
		}
		seen[asset.AssetID] = struct{}{}
		previous = asset.AssetID
	}
	return payload, nil
}
