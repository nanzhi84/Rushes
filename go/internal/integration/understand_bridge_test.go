package integration_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
	"github.com/nanzhi84/Rushes/go/internal/worker"
)

const (
	firstVisionMarker  = "VISION_RESULT_7F3A"
	secondVisionMarker = "VISION_RESULT_9C2D"
)

type markerVisionModel struct {
	mu    sync.Mutex
	calls int
}

func (modelValue *markerVisionModel) WithTools(
	[]*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	return modelValue, nil
}

func (modelValue *markerVisionModel) Generate(
	_ context.Context,
	_ []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	modelValue.mu.Lock()
	defer modelValue.mu.Unlock()
	modelValue.calls++
	switch modelValue.calls {
	case 1:
		return schema.AssistantMessage(
			`{"overall":"第一份素材的独特视觉结论：`+firstVisionMarker+`",`+
				`"semantic_role":"b_roll","segments":[{"id":"s000",`+
				`"description":"蓝色画面，`+firstVisionMarker+`","tags":["蓝色"],`+
				`"quality":"usable","subjects":[],"actions":[],"setting":[],"shot_scale":"全景",`+
				`"composition":"纯色","lighting":[],"mood":[],"edit_hints":[]}]}`,
			nil,
		), nil
	case 2:
		return schema.AssistantMessage(
			`{"overall":"第二份素材的独特视觉结论：`+secondVisionMarker+`",`+
				`"semantic_role":"b_roll","segments":[{"id":"s000",`+
				`"description":"红色画面，`+secondVisionMarker+`","tags":["红色"],`+
				`"quality":"usable","subjects":[],"actions":[],"setting":[],"shot_scale":"全景",`+
				`"composition":"纯色","lighting":[],"mood":[],"edit_hints":[]}]}`,
			nil,
		), nil
	default:
		return nil, errors.New("VLM 收到超出双素材范围的额外调用")
	}
}

func (modelValue *markerVisionModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := modelValue.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (modelValue *markerVisionModel) callCount() int {
	modelValue.mu.Lock()
	defer modelValue.mu.Unlock()
	return modelValue.calls
}

func TestBaseShotIndexesBackfillWithoutModelTool(t *testing.T) {
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	firstPath := filepath.Join(database.Paths.Temporary, "source-one.mp4")
	secondPath := filepath.Join(database.Paths.Temporary, "source-two.mp4")
	assertMarkersAbsent(t, "素材文件名", firstPath+"\n"+secondPath)
	writeVideo(t, firstPath, "blue")
	writeVideo(t, secondPath, "red")
	createUnderstandFixture(t, database, firstPath, secondPath)

	visionModel := &markerVisionModel{}
	registry := worker.NewRegistry()
	if err := worker.RegisterUnderstand(
		registry,
		database,
		understanding.NewAnalyzer(visionModel),
	); err != nil {
		t.Fatal(err)
	}
	runner, err := worker.NewRunner(worker.RunnerConfig{
		Database: database,
		Registry: registry,
		WorkerID: "integration_understand_worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.EnqueueBaseShotIndexBackfill(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	for completed := 0; completed < 2; completed++ {
		worked, runErr := runner.RunOnce(t.Context())
		if runErr != nil || !worked {
			t.Fatalf("base index worker completed=%d worked=%v err=%v", completed, worked, runErr)
		}
	}
	var jobs, snapshots, shots, frames, vectorArtifacts int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM jobs WHERE kind='understand' AND status='succeeded'`,
	).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM shot_index_snapshots WHERE status='ready'`,
	).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if err := database.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM shots`).Scan(&shots); err != nil {
		t.Fatal(err)
	}
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COALESCE(SUM(json_array_length(representative_frames_json)),0) FROM shots`,
	).Scan(&frames); err != nil {
		t.Fatal(err)
	}
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sqlite_master
		WHERE lower(COALESCE(name,'') || ' ' || COALESCE(sql,'')) LIKE '%embedding%'
		   OR lower(COALESCE(name,'') || ' ' || COALESCE(sql,'')) LIKE '%vector%'`,
	).Scan(&vectorArtifacts); err != nil {
		t.Fatal(err)
	}
	if jobs != 2 || snapshots != 2 || shots != 2 || frames != 2 ||
		visionModel.callCount() != 2 || vectorArtifacts != 0 {
		t.Fatalf("jobs=%d snapshots=%d shots=%d frames=%d vlm_calls=%d vector_artifacts=%d",
			jobs, snapshots, shots, frames, visionModel.callCount(), vectorArtifacts)
	}
	for _, assetID := range []string{"asset_visual_one", "asset_visual_two"} {
		asset, getErr := storage.GetAsset(t.Context(), database.Read(), assetID)
		if getErr != nil || asset.UnderstandingStatus != "ready" {
			t.Fatalf("asset=%s status=%s err=%v", assetID, asset.UnderstandingStatus, getErr)
		}
		snapshot, snapshotErr := storage.ReadyShotIndexByContentHash(t.Context(), database.Read(), asset.Hash)
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		indexed, listErr := storage.ListShotIndexShots(t.Context(), database.Read(), snapshot.ID)
		if listErr != nil || len(indexed) != 1 || len(indexed[0].RepresentativeFrames) != 1 ||
			indexed[0].ShotID == "" || indexed[0].BoundaryVersion != 1 || indexed[0].SearchText == "" {
			t.Fatalf("asset=%s snapshot=%#v shots=%#v err=%v", assetID, snapshot, indexed, listErr)
		}
	}
	var observations int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM agent_job_observations",
	).Scan(&observations); err != nil || observations != 0 {
		t.Fatalf("same-turn 不得创建 synthetic observation: count=%d err=%v", observations, err)
	}
}

func createUnderstandFixture(t *testing.T, database *storage.DB, firstPath, secondPath string) {
	t.Helper()
	events := []contracts.Event{{
		Type: "DraftCreated", DraftID: "draft_understand_bridge",
		Payload: map[string]any{"name": "异步素材理解集成测试"},
	}}
	for index, fixture := range []struct {
		assetID string
		path    string
		name    string
		hash    string
	}{
		{assetID: "asset_visual_one", path: firstPath, name: "source-one.mp4", hash: "video-hash-one"},
		{assetID: "asset_visual_two", path: secondPath, name: "source-two.mp4", hash: "video-hash-two"},
	} {
		info, err := os.Stat(fixture.path)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events,
			contracts.Event{Type: "AssetImported", Payload: map[string]any{
				"asset_id": fixture.assetID, "job_id": "fixture_import_" + fixture.assetID,
				"storage_mode": "reference", "reference_path": fixture.path,
				"kind": "video", "source": "local_path", "filename": fixture.name,
				"hash": fixture.hash, "mtime": info.ModTime().UnixNano(), "size": info.Size(),
				"ingest_status": "ready", "usable": true,
			}},
			contracts.Event{Type: "AssetLinked", DraftID: "draft_understand_bridge", Payload: map[string]any{
				"asset_id": fixture.assetID, "note": index,
			}},
		)
	}
	result, err := reducer.Apply(t.Context(), database, events, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("fixture reducer status=%s err=%v", result.Status, err)
	}
}

func writeVideo(t *testing.T, path, fill string) {
	t.Helper()
	if _, err := media.RunCommand(
		t.Context(), "ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c="+fill+":s=64x64:r=5:d=0.4",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", path,
	); err != nil {
		t.Fatal(err)
	}
}

func assertMarkersAbsent(t *testing.T, label, value string) {
	t.Helper()
	for _, marker := range []string{firstVisionMarker, secondVisionMarker} {
		if strings.Contains(value, marker) {
			t.Fatalf("%s 不应包含 VLM marker %q: %s", label, marker, value)
		}
	}
}
