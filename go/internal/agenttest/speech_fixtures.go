package agenttest

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

// InsertSpeechFixtureAsset 插入一个引用式音频素材并链入草稿，供跨包 speech 测试复用。
// agentexec 领域单测与 agent 集成测试共用；PR-B 拆分后该 helper 曾随 speech 测试迁到
// agentexec 的 _test.go，令 integration 标签下的 agent 集成测试跨包不可见而无法编译，
// 故上移到共享测试基建包。probe_json 经真实 media.ProbeFile 生成。
func InsertSpeechFixtureAsset(
	t *testing.T, database *storage.DB, draftID, assetID, path string,
) {
	t.Helper()
	probe, err := media.ProbeFile(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	probeJSON, _ := json.Marshal(probe)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES(?, 'reference', ?, 'audio', 'local_path', ?, ?, 1, ?, 'ready', 'none', 1)`,
		assetID, path, filepath.Base(path), assetID, string(probeJSON),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
		VALUES(?, ?, 'Aroll', ?)`,
		draftID, assetID, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
}
