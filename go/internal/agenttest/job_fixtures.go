package agenttest

import (
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/storage"
)

// InsertJobFixtureAsset creates the minimal persisted asset row needed by job
// lifecycle tests whose subject is scheduling/cancellation rather than media IO.
func InsertJobFixtureAsset(t *testing.T, database *storage.DB, assetID string) {
	t.Helper()
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT OR IGNORE INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES(
			?, 'reference', ?, 'video', 'local_path', ?, ?, 1,
			'{}', 'ready', 'none', 1
		)`,
		assetID, "/tmp/"+assetID+".mp4", assetID+".mp4", "hash_"+assetID,
	); err != nil {
		t.Fatal(err)
	}
}
