package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

type importEntry struct {
	path   string
	relDir *string
}

func (server *Server) ImportLocalMaterialApiDraftsDraftIdMaterialsImportLocalPost(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
) {
	if _, err := storage.GetDraft(request.Context(), server.database.Read(), draftID); errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "draft_not_found")
		return
	} else if err != nil {
		server.internalError(writer, err)
		return
	}
	var payload MaterialImportLocalRequest
	if err := decodeJSON(request, &payload); err != nil {
		writeBadRequest(writer, "invalid_json")
		return
	}
	requested := []string{}
	if payload.Path != nil && strings.TrimSpace(*payload.Path) != "" {
		requested = append(requested, *payload.Path)
	}
	if payload.Paths != nil {
		requested = append(requested, *payload.Paths...)
	}
	if len(requested) == 0 {
		writeBadRequest(writer, "missing_path")
		return
	}
	entries, skipped, reason, forbidden := server.expandImportEntries(requested)
	if reason != "" {
		if forbidden {
			writeJSON(writer, http.StatusForbidden, map[string]any{"detail": map[string]string{"reason": reason}})
		} else {
			writeBadRequest(writer, reason)
		}
		return
	}
	if payload.AssetId != nil && len(entries) != 1 {
		writeBadRequest(writer, "asset_id_requires_single_file")
		return
	}
	mode := string(Reference)
	if payload.StorageMode != nil {
		mode = string(*payload.StorageMode)
	}
	if mode != string(Reference) && mode != string(Copy) {
		writeBadRequest(writer, "invalid_storage_mode")
		return
	}
	response, err := server.importEntries(request.Context(), draftID, entries, skipped, mode, payload.AssetId)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (server *Server) DeleteMaterialApiDraftsDraftIdMaterialsAssetIdDelete(
	writer http.ResponseWriter,
	request *http.Request,
	draftID, assetID string,
) {
	var linked int
	if err := server.database.Read().QueryRowContext(request.Context(), `
		SELECT 1 FROM draft_asset_links WHERE draft_id=? AND asset_id=?`, draftID, assetID,
	).Scan(&linked); errors.Is(err, sql.ErrNoRows) {
		writeNotFound(writer, "asset_not_linked")
		return
	} else if err != nil {
		server.internalError(writer, err)
		return
	}
	result, err := reducer.Apply(request.Context(), server.database, []contracts.Event{{
		Type: "AssetUnlinked", DraftID: draftID, Payload: map[string]any{"asset_id": assetID},
	}}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil {
		server.internalError(writer, err)
		return
	}
	if result.Status != reducer.StatusApplied {
		writeReducerResult(writer, result)
		return
	}
	writeJSON(writer, http.StatusOK, MaterialMutationResponse{
		DraftId: draftID, AssetId: &assetID, EventIds: reducerEventIDs(result),
	})
}

func (server *Server) expandImportEntries(requested []string) ([]importEntry, []string, string, bool) {
	entries := []importEntry{}
	skipped := []string{}
	seen := map[string]struct{}{}
	for _, raw := range requested {
		path, ok := server.allowedPath(raw)
		if !ok {
			return nil, nil, "path_escape", true
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, nil, "path_not_found", false
		}
		if !info.IsDir() {
			if _, ok := materialKind(path); !ok {
				return nil, nil, "unsupported_material_type", false
			}
			if _, duplicate := seen[path]; !duplicate {
				seen[path] = struct{}{}
				entries = append(entries, importEntry{path: path})
			}
			continue
		}
		rootName := filepath.Base(path)
		_ = filepath.WalkDir(path, func(candidate string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				skipped = append(skipped, filepath.Base(candidate)+"（不可读）")
				return nil
			}
			relative, _ := filepath.Rel(path, candidate)
			if relative == "." {
				return nil
			}
			for _, part := range strings.Split(relative, string(filepath.Separator)) {
				if strings.HasPrefix(part, ".") {
					if entry.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
			}
			if entry.IsDir() {
				return nil
			}
			resolved, allowed := server.allowedPath(candidate)
			if !allowed {
				skipped = append(skipped, filepath.ToSlash(relative)+"（越出允许目录）")
				return nil
			}
			fileInfo, statErr := os.Stat(resolved)
			if statErr != nil || !fileInfo.Mode().IsRegular() {
				return nil
			}
			if _, supported := materialKind(resolved); !supported {
				skipped = append(skipped, filepath.ToSlash(relative))
				return nil
			}
			if _, duplicate := seen[resolved]; duplicate {
				return nil
			}
			seen[resolved] = struct{}{}
			parent := filepath.Dir(relative)
			relDir := rootName
			if parent != "." {
				relDir = filepath.ToSlash(filepath.Join(rootName, parent))
			}
			entries = append(entries, importEntry{path: resolved, relDir: &relDir})
			return nil
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	sort.Strings(skipped)
	return entries, skipped, "", false
}

func (server *Server) importEntries(
	ctx context.Context,
	draftID string,
	entries []importEntry,
	skipped []string,
	mode string,
	requestedAssetID *string,
) (MaterialMutationResponse, error) {
	assetIDs := []string{}
	duplicates := []string{}
	failed := []string{}
	eventIDs := []int{}
	var firstJobID *string
	store := media.NewObjectStore(server.database.Paths)
	for _, entry := range entries {
		linked, err := server.assetLinkedByPath(ctx, draftID, entry.path)
		if err != nil {
			return MaterialMutationResponse{}, err
		}
		if linked {
			duplicates = append(duplicates, filepath.Base(entry.path))
			continue
		}
		if existingID, err := server.assetByReferencePath(ctx, entry.path); err != nil {
			return MaterialMutationResponse{}, err
		} else if existingID != "" {
			result, applyErr := reducer.Apply(ctx, server.database, []contracts.Event{{
				Type: "AssetLinked", DraftID: draftID,
				Payload: map[string]any{"asset_id": existingID, "rel_dir": pointerValue(entry.relDir)},
			}}, reducer.Options{Actor: contracts.ActorUser})
			if applyErr != nil {
				return MaterialMutationResponse{}, applyErr
			}
			assetIDs = append(assetIDs, existingID)
			eventIDs = append(eventIDs, reducerEventIDs(result)...)
			continue
		}
		info, err := os.Stat(entry.path)
		if err != nil {
			failed = append(failed, filepath.Base(entry.path)+"（不可读）")
			continue
		}
		kind, _ := materialKind(entry.path)
		assetID := newID("asset")
		if requestedAssetID != nil && *requestedAssetID != "" {
			assetID = *requestedAssetID
		}
		var objectHash string
		var objectSize int64
		var referencePath any = entry.path
		var digest string
		if mode == string(Copy) {
			ref, putErr := store.PutFile(ctx, entry.path)
			if putErr != nil {
				failed = append(failed, filepath.Base(entry.path)+"（拷贝失败）")
				continue
			}
			objectHash, objectSize, digest, referencePath = ref.Hash, ref.Size, ref.Hash, nil
		} else {
			digest, err = hashFile(ctx, entry.path)
			if err != nil {
				failed = append(failed, filepath.Base(entry.path)+"（哈希失败）")
				continue
			}
		}
		jobID := newID("job")
		jobIDCopy := jobID
		if firstJobID == nil {
			firstJobID = &jobIDCopy
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		payload := map[string]any{
			"asset_id": assetID, "job_id": jobID, "storage_mode": mode,
			"reference_path": referencePath, "object_hash": objectHash, "object_size": objectSize,
			"kind": kind, "source": "local_path", "filename": filepath.Base(entry.path),
			"hash": digest, "mtime": info.ModTime().UnixNano(), "size": info.Size(),
			"ingest_status": "imported",
		}
		result, applyErr := reducer.Apply(ctx, server.database, []contracts.Event{
			{Type: "AssetImported", Payload: payload},
			{Type: "AssetLinked", DraftID: draftID, Payload: map[string]any{
				"asset_id": assetID, "rel_dir": pointerValue(entry.relDir),
			}},
			{Type: "JobEnqueued", DraftID: draftID, Payload: map[string]any{
				"job_id": jobID, "kind": "ingest", "asset_id": assetID,
				"requested_by_draft_id": draftID, "idempotency_key": "asset:" + assetID + ":ingest",
				"job_payload": map[string]any{"asset_id": assetID}, "max_retries": 2,
				"next_run_at": now, "priority": 10,
			}},
		}, reducer.Options{Actor: contracts.ActorUser})
		if applyErr != nil {
			return MaterialMutationResponse{}, applyErr
		}
		assetIDs = append(assetIDs, assetID)
		eventIDs = append(eventIDs, reducerEventIDs(result)...)
	}
	var firstAssetID *string
	if len(assetIDs) > 0 {
		firstAssetID = &assetIDs[0]
	}
	return MaterialMutationResponse{
		DraftId: draftID, AssetId: firstAssetID, AssetIds: &assetIDs, JobId: firstJobID,
		Duplicates: &duplicates, Skipped: &skipped, Failed: &failed, EventIds: eventIDs,
	}, nil
}

func (server *Server) ListMaterialsApiDraftsDraftIdMaterialsGet(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
) {
	server.writeMaterials(writer, request, draftID, nil)
}

func (server *Server) RevalidateMaterialsApiDraftsDraftIdMaterialsRevalidatePost(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
) {
	server.writeMaterials(writer, request, draftID, &[]string{})
}

func (server *Server) writeMaterials(
	writer http.ResponseWriter,
	request *http.Request,
	draftID string,
	invalidated *[]string,
) {
	if _, err := storage.GetDraft(request.Context(), server.database.Read(), draftID); errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "draft_not_found")
		return
	} else if err != nil {
		server.internalError(writer, err)
		return
	}
	assets, err := storage.ListDraftAssets(request.Context(), server.database.Read(), draftID)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	items := make([]MaterialAsset, 0, len(assets))
	for _, asset := range assets {
		jobs, err := storage.ListAssetJobs(request.Context(), server.database.Read(), asset.ID)
		if err != nil {
			server.internalError(writer, err)
			return
		}
		items = append(items, materialAsset(asset, jobs))
	}
	writeJSON(writer, http.StatusOK, MaterialsResponse{DraftId: draftID, Assets: items, InvalidatedAssetIds: invalidated})
}

func (server *Server) GetMaterialSummaryApiDraftsDraftIdMaterialsAssetIdSummaryGet(
	writer http.ResponseWriter,
	request *http.Request,
	draftID, assetID string,
) {
	var linked int
	if err := server.database.Read().QueryRowContext(request.Context(), `
		SELECT 1 FROM draft_asset_links WHERE draft_id=? AND asset_id=?`, draftID, assetID).Scan(&linked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeNotFound(writer, "asset_not_linked")
			return
		}
		server.internalError(writer, err)
		return
	}
	summary, err := storage.LatestMaterialSummary(request.Context(), server.database.Read(), assetID)
	if errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "summary_not_ready")
		return
	}
	if err != nil {
		server.internalError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, MaterialSummaryResponse{AssetId: assetID, Summary: summary})
}

func materialAsset(asset storage.Asset, jobs []storage.JobSummary) MaterialAsset {
	jobItems := make([]AssetJobSummary, 0, len(jobs))
	for _, job := range jobs {
		var progress *float32
		if job.Progress != nil {
			value := float32(*job.Progress)
			progress = &value
		}
		jobItems = append(jobItems, AssetJobSummary{
			JobId: job.ID, Kind: job.Kind, Status: job.Status,
			Progress: progress, ErrorJson: mapPointer(job.Error),
		})
	}
	var duration *float32
	if value, ok := numeric(asset.Probe["duration_sec"]); ok {
		converted := float32(value)
		duration = &converted
	}
	var mtime *int
	if asset.MTime != nil {
		value := int(*asset.MTime)
		mtime = &value
	}
	return MaterialAsset{
		AssetId: asset.ID, StorageMode: asset.StorageMode, Kind: asset.Kind,
		Source: asset.Source, Filename: asset.Filename, Hash: asset.Hash, Size: int(asset.Size),
		Mtime: mtime, IngestStatus: asset.IngestStatus,
		UnderstandingStatus: asset.UnderstandingStatus, Usable: asset.Usable,
		RelDir: asset.RelDir, Probe: mapPointer(asset.Probe), DurationSec: duration,
		ProxyObjectHash: asset.ProxyObjectHash, ProxyReady: asset.ProxyObjectHash != nil,
		ThumbnailReady: asset.ThumbnailObjectHash != nil, Invalid: !asset.Usable,
		Failure: mapPointer(asset.Failure), Jobs: jobItems,
	}
}

func (server *Server) assetLinkedByPath(ctx context.Context, draftID, path string) (bool, error) {
	var found int
	err := server.database.Read().QueryRowContext(ctx, `
		SELECT 1 FROM assets a JOIN draft_asset_links l ON l.asset_id=a.asset_id
		WHERE l.draft_id=? AND a.reference_path=? LIMIT 1`, draftID, path).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (server *Server) assetByReferencePath(ctx context.Context, path string) (string, error) {
	var assetID string
	err := server.database.Read().QueryRowContext(ctx,
		"SELECT asset_id FROM assets WHERE reference_path=? ORDER BY asset_id LIMIT 1", path).Scan(&assetID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return assetID, err
}

func materialKind(path string) (string, bool) {
	extension := strings.ToLower(filepath.Ext(path))
	switch extension {
	case ".mp4", ".mov", ".mkv", ".webm", ".avi", ".m4v", ".mpg", ".mpeg", ".3gp", ".wmv":
		return "video", true
	case ".mp3", ".wav", ".m4a", ".aac", ".flac", ".ogg", ".opus", ".aiff", ".aif", ".ape":
		return "audio", true
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".tif", ".tiff", ".heic", ".heif", ".svg":
		return "image", true
	case ".ttf", ".otf", ".woff", ".woff2":
		return "font", true
	default:
		return "", false
	}
}

func hashFile(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	buffer := make([]byte, 1024*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			_, _ = hasher.Write(buffer[:read])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func pointerValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func numeric(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	default:
		return 0, false
	}
}
