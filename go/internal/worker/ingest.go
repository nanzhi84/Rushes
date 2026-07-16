package worker

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func RegisterIngest(registry *Registry, database *storage.DB) error {
	store := media.NewObjectStore(database.Paths)
	return registry.Register("ingest", func(
		ctx context.Context,
		job Job,
		report ProgressReporter,
	) (map[string]any, error) {
		assetID := value(job.AssetID)
		source, kind, err := media.ResolveAssetSource(ctx, database, assetID)
		if err != nil {
			return nil, err
		}
		if err := report(ctx, job, Progress(0.05)); err != nil {
			return nil, err
		}
		probe := media.Probe{}
		if kind != "font" {
			probe, err = media.ProbeFile(ctx, source)
			if err != nil {
				return nil, err
			}
		}
		if err := report(ctx, job, Progress(0.2)); err != nil {
			return nil, err
		}
		thumbnail, err := media.GenerateThumbnail(ctx, store, source, kind)
		if err != nil {
			return nil, err
		}
		probePayload := map[string]any{}
		encoded, _ := json.Marshal(probe)
		_ = json.Unmarshal(encoded, &probePayload)
		assetPayload := map[string]any{
			"asset_id": assetID, "probe": probePayload, "ingest_status": "probed",
		}
		if thumbnail != nil {
			assetPayload["thumbnail_object_hash"] = thumbnail.Hash
			assetPayload["thumbnail_object_size"] = thumbnail.Size
		}
		if kind == "image" || kind == "font" {
			assetPayload["ingest_status"] = "ready"
		}
		if _, err := reducer.Apply(ctx, database, []contracts.Event{{
			Type: "AssetProbed", Payload: assetPayload,
		}}, claimedJobOptions(job, reducer.Options{CreatedAt: time.Now().UTC()})); err != nil {
			return nil, err
		}
		if err := report(ctx, job, Progress(0.45)); err != nil {
			return nil, err
		}
		proxy, err := media.GenerateProxy(ctx, store, source, kind, func(progress media.Progress) {
			fraction := 0.5
			if probe.DurationSec > 0 {
				fraction += min(progress.OutTime.Seconds()/probe.DurationSec, 1) * 0.45
			}
			_ = report(ctx, job, Progress(fraction))
		})
		if err != nil {
			return nil, err
		}
		result := map[string]any{"asset_id": assetID, "probe": probePayload}
		if thumbnail != nil {
			result["thumbnail_object_hash"] = thumbnail.Hash
		}
		if proxy != nil {
			result["proxy_object_hash"] = proxy.Hash
			if _, err := reducer.Apply(ctx, database, []contracts.Event{{
				Type: "ProxyGenerated", Payload: map[string]any{
					"asset_id": assetID, "proxy_object_hash": proxy.Hash,
					"proxy_object_size": proxy.Size, "ingest_status": "ready",
				},
			}}, claimedJobOptions(job, reducer.Options{CreatedAt: time.Now().UTC()})); err != nil {
				return nil, err
			}
		}
		if err := report(ctx, job, Progress(0.99)); err != nil {
			return nil, err
		}
		return result, nil
	})
}
