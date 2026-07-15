//go:build e2e_scaffold

package agent

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func TestUnderstandingMiniLoopCancellationKeepsCompletedSummaryAndResetsPendingAsset(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_understand_cancel")
	source := filepath.Join(database.Paths.Temporary, "understand-cancel.mp4")
	if _, err := media.RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30:duration=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", source); err != nil {
		t.Fatal(err)
	}
	events := []contracts.Event{}
	for index, assetID := range []string{"ready_asset", "slow_asset"} {
		events = append(events,
			contracts.Event{Type: "AssetImported", Payload: map[string]any{
				"asset_id": assetID, "job_id": "job_asset_" + assetID,
				"storage_mode": "reference", "reference_path": source, "kind": "video",
				"source": "local_path", "filename": assetID + ".mp4", "hash": assetID,
				"size": index + 1, "probe": map[string]any{"duration_sec": 1}, "ingest_status": "ready",
			}},
			contracts.Event{Type: "AssetLinked", DraftID: "draft_understand_cancel", Payload: map[string]any{"asset_id": assetID}},
		)
	}
	result, err := reducer.Apply(t.Context(), database, events, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("assets status=%s err=%v", result.Status, err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, stream, unsubscribe := service.Hub().Subscribe("draft_understand_cancel")
	defer unsubscribe()
	service.Queue().EnqueueUserMessage("draft_understand_cancel", "message", e2eCancelUnderstandingMarker)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case event := <-stream:
			completed, _ := event["completed"].(int)
			if event["type"] == "subagent_progress" && completed == 1 {
				if !service.Queue().RequestStop("draft_understand_cancel") {
					t.Fatal("理解进行中取消失败")
				}
				service.Queue().JoinDraft("draft_understand_cancel")
				ready, _ := storage.GetAsset(t.Context(), database.Read(), "ready_asset")
				slow, _ := storage.GetAsset(t.Context(), database.Read(), "slow_asset")
				if ready.UnderstandingStatus != "ready" || slow.UnderstandingStatus != "none" {
					t.Fatalf("ready=%s slow=%s", ready.UnderstandingStatus, slow.UnderstandingStatus)
				}
				if _, err := storage.BestMaterialSummary(t.Context(), database.Read(), "ready_asset"); err != nil {
					t.Fatal(err)
				}
				return
			}
		case <-deadline:
			t.Fatal("等待理解 1/2 超时")
		}
	}
}

func TestE2EFallbackScaffoldDeclinesOrdinaryInput(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if service.fallbackScaffold == nil {
		t.Fatal("e2e_scaffold 构建必须安装 fallback scaffold")
	}
	reply, handled, err := service.fallbackScaffold.TryHandle(
		t.Context(), "missing", "message", "普通产品输入",
	)
	if err != nil || handled || reply != "" {
		t.Fatalf("reply=%q handled=%v err=%v", reply, handled, err)
	}
}
