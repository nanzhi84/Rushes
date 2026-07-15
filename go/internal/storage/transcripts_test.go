package storage

import "testing"

func TestTranscriptQueriesDecodeRowsAndReportMissingOrInvalidJSON(t *testing.T) {
	t.Parallel()
	database, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := LatestTranscript(t.Context(), database.Read(), "missing"); err != ErrNotFound {
		t.Fatalf("missing err=%v", err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES('asset_valid','reference','/tmp/a.wav','audio','local_path','a.wav','hash',1,
			'{}','ready','none',1);
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES('asset_bad','reference','/tmp/b.wav','audio','local_path','b.wav','hash_bad',1,
			'{}','ready','none',1);
		INSERT INTO transcripts(
			transcript_id,asset_id,provider_id,raw_preserved,utterances_json,vad_segments_json
		) VALUES('transcript_valid','asset_valid','sidecar-srt',0,'null','null');
		INSERT INTO transcripts(
			transcript_id,asset_id,provider_id,raw_preserved,utterances_json,vad_segments_json
		) VALUES('transcript_bad','asset_bad','bad',0,'{','[]');
	`); err != nil {
		t.Fatal(err)
	}
	valid, err := LatestTranscript(t.Context(), database.Read(), "asset_valid")
	if err != nil {
		t.Fatal(err)
	}
	if valid.RawPreserved || valid.Utterances == nil || valid.VADSegments == nil ||
		len(valid.Utterances) != 0 || len(valid.VADSegments) != 0 {
		t.Fatalf("valid=%#v", valid)
	}
	if _, err := LatestTranscript(t.Context(), database.Read(), "asset_bad"); err == nil {
		t.Fatal("无效 utterances_json 应失败")
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		UPDATE transcripts SET utterances_json='[]',vad_segments_json='{' WHERE transcript_id='transcript_bad'
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := LatestTranscript(t.Context(), database.Read(), "asset_bad"); err == nil {
		t.Fatal("无效 vad_segments_json 应失败")
	}
}
