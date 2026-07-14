package media

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"

	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func GenerateThumbnail(
	ctx context.Context,
	store ObjectStore,
	source, kind string,
) (*ObjectRef, error) {
	if kind == "audio" || kind == "font" {
		return nil, nil
	}
	temporary, err := os.CreateTemp(store.paths.Temporary, "thumbnail-*.jpg")
	if err != nil {
		return nil, err
	}
	path := temporary.Name()
	_ = temporary.Close()
	defer func() { _ = os.Remove(path) }()
	args := []string{"-y", "-i", source, "-frames:v", "1", "-vf", "scale='min(480,iw)':-2", "-q:v", "3", path}
	if err := RunFFmpegProgress(ctx, "ffmpeg", args, nil); err != nil {
		return nil, err
	}
	ref, err := store.PutFile(ctx, path)
	return &ref, err
}

func GenerateProxy(
	ctx context.Context,
	store ObjectStore,
	source, kind string,
	onProgress func(Progress),
) (*ObjectRef, error) {
	if kind == "image" || kind == "font" {
		return nil, nil
	}
	extension := ".mp4"
	args := []string{
		"-y", "-i", source, "-map", "0:v:0", "-map", "0:a?",
		"-vf", "scale=1280:720:force_original_aspect_ratio=decrease:force_divisible_by=2",
		"-r", "30", "-fps_mode", "cfr",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p",
		"-g", "30", "-keyint_min", "30", "-sc_threshold", "0",
		"-movflags", "+faststart", "-c:a", "aac", "-b:a", "128k",
	}
	if kind == "audio" {
		extension = ".mp3"
		args = []string{"-y", "-i", source, "-vn", "-c:a", "libmp3lame", "-b:a", "96k"}
	}
	temporary, err := os.CreateTemp(store.paths.Temporary, "proxy-*"+extension)
	if err != nil {
		return nil, err
	}
	path := temporary.Name()
	_ = temporary.Close()
	defer func() { _ = os.Remove(path) }()
	args = append(args, path)
	if err := RunFFmpegProgress(ctx, "ffmpeg", args, onProgress); err != nil {
		return nil, err
	}
	ref, err := store.PutFile(ctx, path)
	return &ref, err
}

func ResolveAssetSource(ctx context.Context, database *storage.DB, assetID string) (string, string, error) {
	var mode, kind string
	var objectHash, referencePath sql.NullString
	var usable int
	err := database.Read().QueryRowContext(ctx, `
		SELECT storage_mode, kind, object_hash, reference_path, usable
		FROM assets WHERE asset_id=?`, assetID).Scan(&mode, &kind, &objectHash, &referencePath, &usable)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", storage.ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	if usable == 0 {
		return "", "", errors.New("素材已失效")
	}
	if mode == "reference" && referencePath.Valid {
		path := filepath.Clean(referencePath.String)
		if _, err := os.Stat(path); err != nil {
			return "", "", err
		}
		return path, kind, nil
	}
	if mode == "copy" && objectHash.Valid {
		path, err := database.Paths.ObjectPath(objectHash.String)
		return path, kind, err
	}
	return "", "", errors.New("素材没有可读源文件")
}
