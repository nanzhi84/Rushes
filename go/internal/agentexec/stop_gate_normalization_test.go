package agentexec

import (
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

func TestBestStopGateIndexedShotAndDefaultPlaybackRate(t *testing.T) {
	shots := []storage.DraftIndexedShot{
		{AssetID: "other", Shot: storage.IndexedShot{
			ShotID: "shot_other", SourceStartFrame: 0, SourceEndFrame: 100,
		}},
		{AssetID: "asset", Shot: storage.IndexedShot{
			ShotID: "shot_short", SourceStartFrame: 0, SourceEndFrame: 10,
		}},
		{AssetID: "asset", Shot: storage.IndexedShot{
			ShotID: "shot_best", SourceStartFrame: 5, SourceEndFrame: 40,
		}},
	}
	best := bestStopGateIndexedShot(shots, "asset", 8, 35)
	if best == nil || best.Shot.ShotID != "shot_best" {
		t.Fatalf("best=%#v", best)
	}
	if bestStopGateIndexedShot(shots, "missing", 0, 20) != nil {
		t.Fatal("unknown asset must not resolve to an indexed shot")
	}
	if rate := effectiveTimelinePlaybackRate(timeline.Clip{}); rate != 1 {
		t.Fatalf("default rate=%v", rate)
	}
	if rate := effectiveTimelinePlaybackRate(timeline.Clip{PlaybackRate: 0.5}); rate != 0.5 {
		t.Fatalf("explicit rate=%v", rate)
	}
}
