package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

var mediaExtensions = map[string]struct{}{
	".mp4": {}, ".mov": {}, ".mkv": {}, ".webm": {}, ".avi": {}, ".m4v": {}, ".mpg": {}, ".mpeg": {}, ".3gp": {}, ".wmv": {},
	".mp3": {}, ".wav": {}, ".m4a": {}, ".aac": {}, ".flac": {}, ".ogg": {}, ".opus": {}, ".aiff": {}, ".aif": {}, ".ape": {},
	".jpg": {}, ".jpeg": {}, ".png": {}, ".gif": {}, ".webp": {}, ".bmp": {}, ".tif": {}, ".tiff": {}, ".heic": {}, ".heif": {}, ".svg": {},
	".ttf": {}, ".otf": {}, ".woff": {}, ".woff2": {},
}

func defaultFSRoots() []string {
	home, _ := os.UserHomeDir()
	return canonicalRoots([]string{home, filepath.Join(home, "Movies"), filepath.Join(home, "Desktop"), "/Volumes"})
}

func (server *Server) FsRootsApiFsRootsGet(writer http.ResponseWriter, _ *http.Request) {
	roots := make([]FsRoot, 0, len(server.fsRoots))
	for _, root := range server.fsRoots {
		_, err := os.Stat(root)
		name := filepath.Base(root)
		if root == "/" {
			name = "/"
		}
		roots = append(roots, FsRoot{Path: root, Name: name, Exists: err == nil})
	}
	writeJSON(writer, http.StatusOK, FsRootsResponse{Roots: roots})
}

func (server *Server) FsListApiFsListGet(
	writer http.ResponseWriter,
	request *http.Request,
	params FsListApiFsListGetParams,
) {
	path, ok := server.allowedPath(params.Path)
	if !ok {
		writeJSON(writer, http.StatusForbidden, map[string]any{
			"detail": map[string]string{"reason": "path_escape"},
		})
		return
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		writeNotFound(writer, "not_found")
		return
	}
	directoryEntries, err := os.ReadDir(path)
	if err != nil {
		server.internalError(writer, err)
		return
	}
	entries := make([]FsListEntry, 0, len(directoryEntries))
	for _, entry := range directoryEntries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		entryType := FsListEntryType("directory")
		var extension *string
		var size *int
		if !entry.IsDir() {
			ext := strings.ToLower(filepath.Ext(entry.Name()))
			if _, supported := mediaExtensions[ext]; !supported {
				continue
			}
			entryType = FsListEntryType("file")
			extension = &ext
			if info, err := entry.Info(); err == nil {
				value := int(info.Size())
				size = &value
			}
		}
		entries = append(entries, FsListEntry{
			Name: entry.Name(), Path: filepath.Join(path, entry.Name()),
			Type: entryType, Extension: extension, Size: size,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Type == entries[j].Type {
			return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
		}
		return entries[i].Type == FsListEntryType("directory")
	})
	writeJSON(writer, http.StatusOK, FsListResponse{Path: path, Entries: entries})
}

func (server *Server) FsPickApiFsPickPost(writer http.ResponseWriter, request *http.Request) {
	var payload FsPickRequest
	if err := decodeJSON(request, &payload); err != nil {
		writeBadRequest(writer, "invalid_json")
		return
	}
	mode := "files"
	if payload.Mode != nil {
		mode = string(*payload.Mode)
	}
	if mode != "files" && mode != "folder" {
		writeBadRequest(writer, "invalid_picker_mode")
		return
	}
	paths, available := server.picker(request.Context(), mode)
	writeJSON(writer, http.StatusOK, FsPickResponse{Available: available, Paths: &paths})
}

func (server *Server) allowedPath(raw string) (string, bool) {
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return "", false
	}
	absolute = filepath.Clean(absolute)
	if evaluated, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = evaluated
	}
	for _, root := range server.fsRoots {
		relative, err := filepath.Rel(root, absolute)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return absolute, true
		}
	}
	return "", false
}

func nativePicker(ctx context.Context, mode string) ([]string, bool) {
	return nativePickerWith(
		ctx,
		mode,
		runtime.GOOS,
		exec.LookPath,
		func(ctx context.Context, script string) ([]byte, error) {
			return exec.CommandContext(ctx, "osascript", "-e", script).CombinedOutput()
		},
	)
}

func nativePickerWith(
	ctx context.Context,
	mode string,
	goos string,
	lookPath func(string) (string, error),
	run func(context.Context, string) ([]byte, error),
) ([]string, bool) {
	if goos != "darwin" {
		return []string{}, false
	}
	if _, err := lookPath("osascript"); err != nil {
		return []string{}, false
	}
	command := "choose file with prompt \"选择要导入的素材\" with multiple selections allowed"
	if mode == "folder" {
		command = "choose folder with prompt \"选择要导入的素材文件夹（可多选）\" with multiple selections allowed"
	}
	script := "with timeout of 3600 seconds\n" +
		"set picked to " + command + "\n" +
		"set out to \"\"\nrepeat with f in picked\n" +
		"set out to out & POSIX path of f & linefeed\nend repeat\nreturn out\nend timeout"
	output, err := run(ctx, script)
	if err != nil {
		if strings.Contains(string(output), "-128") || errors.Is(err, context.Canceled) {
			return []string{}, true
		}
		return []string{}, false
	}
	var paths []string
	for _, line := range strings.Split(string(output), "\n") {
		if path := strings.TrimSpace(line); path != "" {
			paths = append(paths, path)
		}
	}
	return paths, true
}
