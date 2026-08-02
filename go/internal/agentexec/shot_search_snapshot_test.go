package agentexec

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func TestShotSearchSnapshotTokenIsDeterministicAndTamperEvident(t *testing.T) {
	values := []frozenShotAsset{
		{asset: storage.Asset{ID: "asset_b", Hash: "hash_b"}, snapshot: storage.ShotIndexSnapshot{ID: "base_b", Generation: 2}},
		{asset: storage.Asset{ID: "asset_a", Hash: "hash_a"}, snapshot: storage.ShotIndexSnapshot{ID: "base_a", Generation: 1}},
	}
	token := shotSearchSnapshotID("draft_snapshot", values)
	reversed := shotSearchSnapshotID("draft_snapshot", []frozenShotAsset{values[1], values[0]})
	if token != reversed || !strings.HasPrefix(token, shotSearchSnapshotPrefix) {
		t.Fatalf("snapshot token 不稳定: first=%q reversed=%q", token, reversed)
	}
	parsed, err := parseShotSearchSnapshot(token)
	if err != nil || parsed.DraftID != "draft_snapshot" || len(parsed.Assets) != 2 ||
		parsed.Assets[0].AssetID != "asset_a" || parsed.Assets[1].Generation != 2 {
		t.Fatalf("parsed=%#v err=%v", parsed, err)
	}

	last := token[len(token)-1]
	replacement := byte('0')
	if last == replacement {
		replacement = '1'
	}
	if _, err := parseShotSearchSnapshot(token[:len(token)-1] + string(replacement)); err == nil {
		t.Fatal("篡改 checksum 的 snapshot token 不应被接受")
	}
	if _, err := parseShotSearchSnapshot("legacy_snapshot_id"); err == nil {
		t.Fatal("旧版不自证 snapshot id 不应被静默接受")
	}
}

func TestShotSearchSnapshotParserRejectsMalformedPayloads(t *testing.T) {
	signed := func(value any) string {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(encoded)
		return shotSearchSnapshotPrefix + base64.RawURLEncoding.EncodeToString(encoded) + "." +
			hex.EncodeToString(digest[:16])
	}
	validAsset := shotSearchSnapshotAsset{
		AssetID: "asset", ContentHash: "hash", BaseSnapshotID: "base", Generation: 1,
	}
	for name, token := range map[string]string{
		"bad structure": shotSearchSnapshotPrefix + "only-one-part",
		"bad base64":    shotSearchSnapshotPrefix + "!.00000000000000000000000000000000",
		"bad hex":       shotSearchSnapshotPrefix + "e30.zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
		"invalid json": func() string {
			encoded := []byte("not-json")
			digest := sha256.Sum256(encoded)
			return shotSearchSnapshotPrefix + base64.RawURLEncoding.EncodeToString(encoded) + "." +
				hex.EncodeToString(digest[:16])
		}(),
		"missing draft": signed(shotSearchSnapshot{
			SynonymVersion: shotSearchSynonymVersion, Assets: []shotSearchSnapshotAsset{validAsset},
		}),
		"invalid asset": signed(shotSearchSnapshot{
			DraftID: "draft", SynonymVersion: shotSearchSynonymVersion,
			Assets: []shotSearchSnapshotAsset{{AssetID: "asset", ContentHash: "hash", Generation: 0}},
		}),
		"duplicate asset": signed(shotSearchSnapshot{
			DraftID: "draft", SynonymVersion: shotSearchSynonymVersion,
			Assets: []shotSearchSnapshotAsset{validAsset, validAsset},
		}),
		"unsorted assets": signed(shotSearchSnapshot{
			DraftID: "draft", SynonymVersion: shotSearchSynonymVersion,
			Assets: []shotSearchSnapshotAsset{
				{AssetID: "b", ContentHash: "hash", BaseSnapshotID: "base_b", Generation: 1},
				{AssetID: "a", ContentHash: "hash", BaseSnapshotID: "base_a", Generation: 1},
			},
		}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseShotSearchSnapshot(token); err == nil {
				t.Fatalf("malformed token accepted: %q", token)
			}
		})
	}
}
