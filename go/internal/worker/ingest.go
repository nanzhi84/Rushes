package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
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
		// 波形峰值：仅对含音频的资产（音频 asset 或带音轨的视频）生成并挂到对象库，
		// 供前端直接绘制而不再下载解码整段音频。best-effort——生成失败则该资产无 peaks，
		// 前端回退到即时解码，不阻断已 ready 的 ingest。
		if kind == "audio" || probe.HasAudio {
			if ref := analyzeAndStorePeaks(ctx, store, assetID, source, probe.DurationSec); ref != nil {
				result["peaks_object_hash"] = ref.Hash
				if _, err := reducer.Apply(ctx, database, []contracts.Event{{
					Type: "PeaksGenerated", Payload: map[string]any{
						"asset_id": assetID, "peaks_object_hash": ref.Hash,
						"peaks_object_size": ref.Size,
					},
				}}, claimedJobOptions(job, reducer.Options{CreatedAt: time.Now().UTC()})); err != nil {
					return nil, err
				}
			}
		}
		if err := report(ctx, job, Progress(0.99)); err != nil {
			return nil, err
		}
		return result, nil
	})
}

// analyzeAndStorePeaks 生成波形峰值 JSON 并写入对象库；任何生成/写入失败都返回 nil
// （best-effort，前端回退即时解码），不返回 error 以免阻断 ingest。
func analyzeAndStorePeaks(
	ctx context.Context,
	store media.ObjectStore,
	assetID string,
	source string,
	durationSec float64,
) *media.ObjectRef {
	peaks, err := media.AnalyzeWaveformPeaks(ctx, source, durationSec)
	if err != nil {
		slog.Warn("波形峰值生成失败", "asset_id", assetID, "stage", "analyze", "err", err)
		return nil
	}
	encoded, err := json.Marshal(peaks)
	if err != nil {
		slog.Warn("波形峰值序列化失败", "asset_id", assetID, "stage", "marshal", "err", err)
		return nil
	}
	ref, err := store.Put(ctx, bytes.NewReader(encoded))
	if err != nil {
		slog.Warn("波形峰值写入对象库失败", "asset_id", assetID, "stage", "store", "err", err)
		return nil
	}
	return &ref
}
