package api

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func (server *Server) MediaSourceApiMediaAssetIdSourceGet(writer http.ResponseWriter, request *http.Request, assetID string) {
	server.serveAssetMedia(writer, request, assetID, "source")
}

func (server *Server) MediaSourceApiMediaAssetIdSourceHead(writer http.ResponseWriter, request *http.Request, assetID string) {
	server.serveAssetMedia(writer, request, assetID, "source")
}

func (server *Server) MediaProxyApiMediaAssetIdProxyGet(writer http.ResponseWriter, request *http.Request, assetID string) {
	server.serveAssetMedia(writer, request, assetID, "proxy")
}

func (server *Server) MediaProxyApiMediaAssetIdProxyHead(writer http.ResponseWriter, request *http.Request, assetID string) {
	server.serveAssetMedia(writer, request, assetID, "proxy")
}

func (server *Server) MediaThumbnailApiMediaAssetIdThumbnailGet(writer http.ResponseWriter, request *http.Request, assetID string) {
	server.serveAssetMedia(writer, request, assetID, "thumbnail")
}

func (server *Server) MediaThumbnailApiMediaAssetIdThumbnailHead(writer http.ResponseWriter, request *http.Request, assetID string) {
	server.serveAssetMedia(writer, request, assetID, "thumbnail")
}

func (server *Server) serveAssetMedia(writer http.ResponseWriter, request *http.Request, assetID, variant string) {
	asset, err := storage.GetAsset(request.Context(), server.database.Read(), assetID)
	if errors.Is(err, storage.ErrNotFound) {
		writeNotFound(writer, "asset_not_found")
		return
	}
	if err != nil {
		server.internalError(writer, err)
		return
	}
	var path, contentType string
	switch variant {
	case "source":
		if !asset.Usable {
			writeNotFound(writer, "reference_invalidated")
			return
		}
		if asset.ReferencePath != nil {
			path = *asset.ReferencePath
		} else if asset.ObjectHash != nil {
			path, err = server.database.Paths.ObjectPath(*asset.ObjectHash)
		}
		contentType = contentTypeForName(asset.Filename, "application/octet-stream")
	case "proxy":
		path, err = storage.ObjectPathByHash(server.database.Paths, asset.ProxyObjectHash)
		if asset.Kind == "audio" {
			contentType = "audio/mpeg"
		} else {
			contentType = "video/mp4"
		}
	case "thumbnail":
		path, err = storage.ObjectPathByHash(server.database.Paths, asset.ThumbnailObjectHash)
		contentType = "image/jpeg"
	}
	if err != nil || path == "" {
		writeNotFound(writer, variant+"_not_ready")
		return
	}
	serveRange(writer, request, path, contentType)
}

func (server *Server) MediaPreviewApiMediaPreviewPreviewIdGet(writer http.ResponseWriter, request *http.Request, previewID string) {
	server.serveArtifact(writer, request, "previews", "preview_id", previewID, "preview")
}

func (server *Server) MediaPreviewApiMediaPreviewPreviewIdHead(writer http.ResponseWriter, request *http.Request, previewID string) {
	server.serveArtifact(writer, request, "previews", "preview_id", previewID, "preview")
}

func (server *Server) MediaExportApiMediaExportExportIdGet(writer http.ResponseWriter, request *http.Request, exportID string) {
	server.serveArtifact(writer, request, "exports", "export_id", exportID, "export")
}

func (server *Server) MediaExportApiMediaExportExportIdHead(writer http.ResponseWriter, request *http.Request, exportID string) {
	server.serveArtifact(writer, request, "exports", "export_id", exportID, "export")
}

func (server *Server) serveArtifact(
	writer http.ResponseWriter,
	request *http.Request,
	table, idColumn, id, label string,
) {
	var hash string
	query := fmt.Sprintf("SELECT object_hash FROM %s WHERE %s=?", table, idColumn) //nolint:gosec // identifiers are constants
	err := server.database.Read().QueryRowContext(request.Context(), query, id).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		writeNotFound(writer, label+"_not_found")
		return
	}
	if err != nil {
		server.internalError(writer, err)
		return
	}
	path, err := server.database.Paths.ObjectPath(hash)
	if err != nil {
		writeNotFound(writer, label+"_not_ready")
		return
	}
	serveRange(writer, request, path, "video/mp4")
}

func serveRange(writer http.ResponseWriter, request *http.Request, path, contentType string) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		writeNotFound(writer, "media_not_found")
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"detail": map[string]string{"reason": "media_open_failed"}})
		return
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeNotFound(writer, "media_not_found")
		return
	}
	start, end, partial, rangeErr := parseRange(request.Header.Get("Range"), info.Size())
	if rangeErr != nil {
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", info.Size()))
		writeJSON(writer, http.StatusRequestedRangeNotSatisfiable, map[string]any{
			"detail": map[string]string{"reason": "invalid_range"},
		})
		return
	}
	length := max(int64(0), end-start+1)
	writer.Header().Set("Accept-Ranges", "bytes")
	writer.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	writer.Header().Set("Content-Type", contentType)
	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, info.Size()))
	}
	writer.WriteHeader(status)
	if request.Method == http.MethodHead || length == 0 {
		return
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return
	}
	_, _ = io.CopyN(writer, file, length)
}

func parseRange(header string, size int64) (int64, int64, bool, error) {
	if header == "" {
		return 0, size - 1, false, nil
	}
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, false, errors.New("range unit 必须是 bytes")
	}
	spec := strings.TrimSpace(strings.TrimPrefix(header, "bytes="))
	if strings.Contains(spec, ",") || !strings.Contains(spec, "-") {
		return 0, 0, false, errors.New("不支持多段 Range")
	}
	rawStart, rawEnd, _ := strings.Cut(spec, "-")
	var start, end int64
	var err error
	if rawStart == "" {
		var suffix int64
		suffix, err = strconv.ParseInt(rawEnd, 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false, errors.New("无效后缀 Range")
		}
		start = max(size-suffix, 0)
		end = size - 1
	} else {
		start, err = strconv.ParseInt(rawStart, 10, 64)
		if err != nil {
			return 0, 0, false, err
		}
		if rawEnd == "" {
			end = size - 1
		} else {
			end, err = strconv.ParseInt(rawEnd, 10, 64)
			if err != nil {
				return 0, 0, false, err
			}
		}
	}
	if size <= 0 || start < 0 || end < start || start >= size {
		return 0, 0, false, errors.New("range 越界")
	}
	return start, min(end, size-1), true, nil
}

func contentTypeForName(name, fallback string) string {
	if value := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); value != "" {
		return value
	}
	return fallback
}
