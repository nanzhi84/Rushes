package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// countingQuerier 统计 QueryContext 调用次数,验证批量查询把每 asset 的查询降到常数次。
type countingQuerier struct {
	Querier
	queries int
}

func (counter *countingQuerier) QueryContext(
	ctx context.Context, query string, args ...any,
) (*sql.Rows, error) {
	counter.queries++
	return counter.Querier.QueryContext(ctx, query, args...)
}

// H6:批量取 best-summary/latest-transcript 与逐 asset 单查结果逐字节等价,且查询数与素材
// 数解耦——N 个 asset 各 1 次 QueryContext(共 2),而非逐 asset 的 2N 次。
func TestBatchMaterialLookupsMatchSingleAndCutQueries(t *testing.T) {
	t.Parallel()
	database, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	const assetCount = 100
	now := "2026-07-18T00:00:00Z"
	assetIDs := make([]string, 0, assetCount)
	for index := 0; index < assetCount; index++ {
		assetID := fmt.Sprintf("asset_%03d", index)
		assetIDs = append(assetIDs, assetID)
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO assets(asset_id,storage_mode,kind,source,filename,hash,size,ingest_status,understanding_status,usable)
			VALUES(?,'reference','video','local','clip.mp4',?,12,'ready','ready',1)`,
			assetID, assetID+"_hash",
		); err != nil {
			t.Fatal(err)
		}
		// 每 asset 两个 ready 摘要版本(rich 段更多、分更高),验证批量逐 asset 选优与单条一致。
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO material_summaries(summary_id,asset_id,version,status,summary_json,fingerprint,created_at)
			VALUES
			(?,?,1,'ready','{"overall":"shallow","segments":[{"description":"人物","tags":["人物"]}]}',?,?),
			(?,?,2,'ready','{"overall":"rich","segments":[{"description":"火焰","tags":["火焰","人物"]},{"description":"海边","tags":["海边"]}]}',?,?)`,
			assetID+"_s1", assetID, assetID+"_fp1", now,
			assetID+"_s2", assetID, assetID+"_fp2", now,
		); err != nil {
			t.Fatal(err)
		}
		// 偶数 asset 才有转写,验证「无转写不入 map」与单条 ErrNotFound 一致。
		if index%2 == 0 {
			if _, err := database.Write().ExecContext(t.Context(), `
				INSERT INTO transcripts(transcript_id,asset_id,provider_id,raw_preserved,utterances_json,vad_segments_json)
				VALUES(?,?,'volc',1,'[{"text":"你好"}]','[]')`,
				assetID+"_t", assetID,
			); err != nil {
				t.Fatal(err)
			}
		}
	}

	summaries, err := BestMaterialSummariesForAssets(t.Context(), database.Read(), assetIDs)
	if err != nil {
		t.Fatal(err)
	}
	transcripts, err := LatestTranscriptsForAssets(t.Context(), database.Read(), assetIDs)
	if err != nil {
		t.Fatal(err)
	}
	for _, assetID := range assetIDs {
		single, singleErr := BestMaterialSummary(t.Context(), database.Read(), assetID)
		if singleErr != nil {
			t.Fatalf("single summary %s: %v", assetID, singleErr)
		}
		if !reflect.DeepEqual(summaries[assetID], single) {
			t.Fatalf("summary mismatch %s: batch=%#v single=%#v", assetID, summaries[assetID], single)
		}
		singleTranscript, transcriptErr := LatestTranscript(t.Context(), database.Read(), assetID)
		if errors.Is(transcriptErr, ErrNotFound) {
			if _, ok := transcripts[assetID]; ok {
				t.Fatalf("batch 有转写但单条 %s 判无", assetID)
			}
			continue
		}
		if transcriptErr != nil {
			t.Fatalf("single transcript %s: %v", assetID, transcriptErr)
		}
		if !reflect.DeepEqual(transcripts[assetID], singleTranscript) {
			t.Fatalf("transcript mismatch %s", assetID)
		}
	}

	summaryCounter := &countingQuerier{Querier: database.Read()}
	if _, err := BestMaterialSummariesForAssets(t.Context(), summaryCounter, assetIDs); err != nil {
		t.Fatal(err)
	}
	transcriptCounter := &countingQuerier{Querier: database.Read()}
	if _, err := LatestTranscriptsForAssets(t.Context(), transcriptCounter, assetIDs); err != nil {
		t.Fatal(err)
	}
	if summaryCounter.queries != 1 || transcriptCounter.queries != 1 {
		t.Fatalf("批量应各 1 次 QueryContext(共 2),实际 summary=%d transcript=%d;逐 asset 会是 %d",
			summaryCounter.queries, transcriptCounter.queries, 2*assetCount)
	}
}
