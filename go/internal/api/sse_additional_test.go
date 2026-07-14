package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
)

type controlledSSEWriter struct {
	header   http.Header
	writeErr error
	flushErr error
}

func (writer *controlledSSEWriter) Header() http.Header {
	if writer.header == nil {
		writer.header = make(http.Header)
	}
	return writer.header
}

func (*controlledSSEWriter) WriteHeader(int) {}

func (writer *controlledSSEWriter) Write(payload []byte) (int, error) {
	if writer.writeErr != nil {
		return 0, writer.writeErr
	}
	return len(payload), nil
}

func (writer *controlledSSEWriter) FlushError() error { return writer.flushErr }

func TestStreamEventsCoversReplayRecoveryAndTransportStops(t *testing.T) {
	t.Run("skip malformed persisted event and continue replay", func(t *testing.T) {
		server, handler := testServer(t, t.TempDir(), 1)
		createDraftThroughAPI(t, handler, "draft_before_bad_event")
		var cursor int64
		if err := server.database.Read().QueryRowContext(
			t.Context(), "SELECT MAX(event_id) FROM event_log",
		).Scan(&cursor); err != nil {
			t.Fatal(err)
		}
		if _, err := server.database.Write().ExecContext(t.Context(), `
			INSERT INTO event_log(event_type, actor, payload_json, created_at)
			VALUES('DraftCreated', 'user', '{', ?)`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		createDraftThroughAPI(t, handler, "draft_after_bad_event")

		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(
			http.MethodGet,
			"/api/events?token="+testToken+"&last_event_id="+fmtInt64(cursor),
			nil,
		)
		request.Host = "127.0.0.1:8000"
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK ||
			!strings.Contains(recorder.Body.String(), "draft_after_bad_event") {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("request cancellation", func(t *testing.T) {
		server, _ := testServer(t, t.TempDir(), 0)
		ctx, cancel := context.WithCancel(t.Context())
		request := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
		done := make(chan struct{})
		go func() {
			server.streamEvents(httptest.NewRecorder(), request, nil, nil, contracts.RoutesToWorkspace)
			close(done)
		}()
		time.Sleep(20 * time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("SSE did not stop after request cancellation")
		}
	})

	t.Run("live turn event", func(t *testing.T) {
		server, handler := testServer(t, t.TempDir(), 1)
		createDraftThroughAPI(t, handler, "draft_live_turn")
		draftID := "draft_live_turn"
		request := httptest.NewRequest(http.MethodGet, "/api/events", nil)
		request.Header.Set("Last-Event-ID", "999999")
		recorder := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			server.streamEvents(recorder, request, nil, &draftID, func(contracts.Event) bool { return false })
			close(done)
		}()
		time.Sleep(50 * time.Millisecond)
		server.agent.Hub().Record(draftID, map[string]any{
			"type": "text_delta", "message_id": "live", "delta": "继续",
		})
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("SSE did not forward the live turn event")
		}
		if !strings.Contains(recorder.Body.String(), "event: turn_stream") ||
			!strings.Contains(recorder.Body.String(), "继续") {
			t.Fatalf("body=%s", recorder.Body.String())
		}
	})

	t.Run("turn event encoding error", func(t *testing.T) {
		server, handler := testServer(t, t.TempDir(), 0)
		createDraftThroughAPI(t, handler, "draft_bad_turn_event")
		draftID := "draft_bad_turn_event"
		server.agent.Hub().Record(draftID, map[string]any{"type": "text_delta", "bad": make(chan int)})
		request := httptest.NewRequest(http.MethodGet, "/api/events", nil)
		request.Header.Set("Last-Event-ID", "999999")
		server.streamEvents(
			httptest.NewRecorder(), request, nil, &draftID,
			func(contracts.Event) bool { return false },
		)
	})

	for _, testCase := range []struct {
		name     string
		writeErr error
		flushErr error
	}{
		{name: "writer error", writeErr: errors.New("write failed")},
		{name: "flush error", flushErr: errors.New("flush failed")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server, handler := testServer(t, t.TempDir(), 0)
			createDraftThroughAPI(t, handler, "draft_transport_stop")
			server.streamEvents(
				&controlledSSEWriter{writeErr: testCase.writeErr, flushErr: testCase.flushErr},
				httptest.NewRequest(http.MethodGet, "/api/events", nil),
				nil,
				nil,
				contracts.RoutesToWorkspace,
			)
		})
	}

	t.Run("database error", func(t *testing.T) {
		server, _ := testServer(t, t.TempDir(), 0)
		if err := server.database.Close(); err != nil {
			t.Fatal(err)
		}
		server.streamEvents(
			httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/events", nil),
			nil, nil, contracts.RoutesToWorkspace,
		)
	})
}

func TestFilesystemPickerAndListingAdditionalBranches(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".hidden.mp4"), []byte("hidden"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing-target"), filepath.Join(root, "broken.mp4")); err != nil {
		t.Fatal(err)
	}
	server, handler := testServer(t, root, 0)

	list := httptest.NewRecorder()
	handler.ServeHTTP(list, apiRequest(t, http.MethodGet, "/api/fs/list?path="+root, nil))
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), ".hidden.mp4") ||
		!strings.Contains(list.Body.String(), "broken.mp4") {
		t.Fatalf("status=%d body=%s", list.Code, list.Body.String())
	}

	filePath := filepath.Join(root, "clip.mp4")
	if err := os.WriteFile(filePath, []byte("clip"), 0o644); err != nil {
		t.Fatal(err)
	}
	notDirectory := httptest.NewRecorder()
	handler.ServeHTTP(notDirectory, apiRequest(t, http.MethodGet, "/api/fs/list?path="+filePath, nil))
	if notDirectory.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", notDirectory.Code, notDirectory.Body.String())
	}

	invalidMode := httptest.NewRecorder()
	handler.ServeHTTP(invalidMode, apiRequest(t, http.MethodPost, "/api/fs/pick", map[string]any{"mode": "invalid"}))
	if invalidMode.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", invalidMode.Code, invalidMode.Body.String())
	}

	server.pickerMu.Lock()
	busy := httptest.NewRecorder()
	handler.ServeHTTP(busy, apiRequest(t, http.MethodPost, "/api/fs/pick", map[string]any{"mode": "files"}))
	server.pickerMu.Unlock()
	if busy.Code != http.StatusOK || !strings.Contains(busy.Body.String(), `"paths":[]`) {
		t.Fatalf("status=%d body=%s", busy.Code, busy.Body.String())
	}

	rootServer, rootHandler := testServer(t, "/", 0)
	_ = rootServer
	roots := httptest.NewRecorder()
	rootHandler.ServeHTTP(roots, apiRequest(t, http.MethodGet, "/api/fs/roots", nil))
	if roots.Code != http.StatusOK || !strings.Contains(roots.Body.String(), `"name":"/"`) {
		t.Fatalf("status=%d body=%s", roots.Code, roots.Body.String())
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	paths, available := nativePicker(ctx, "files")
	if runtime.GOOS == "darwin" && (!available || len(paths) != 0) {
		t.Fatalf("paths=%v available=%v", paths, available)
	}
}

func fmtInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}
