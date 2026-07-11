package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"

	"github.com/go-chi/chi/v5"
	"github.com/nanzhi84/Rushes/go/internal/agent"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

type Picker func(context.Context, string) ([]string, bool)

type Config struct {
	Database     *storage.DB
	Token        string
	Port         int
	FSRoots      []string
	SSEMaxEvents int
	Logger       *slog.Logger
	Picker       Picker
	Agent        *agent.Service
}

type Server struct {
	Unimplemented
	database     *storage.DB
	token        string
	port         int
	fsRoots      []string
	sseMaxEvents int
	logger       *slog.Logger
	picker       Picker
	agent        *agent.Service
	ownsAgent    bool
}

var _ ServerInterface = (*Server)(nil)

func NewServer(config Config) (*Server, error) {
	if config.Database == nil {
		return nil, errors.New("API 缺少数据库")
	}
	if config.Port <= 0 || config.Port > 65535 {
		return nil, errors.New("API 端口无效")
	}
	if config.Token == "" {
		config.Token = GenerateToken()
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.Picker == nil {
		config.Picker = nativePicker
	}
	ownedAgent := false
	if config.Agent == nil {
		var err error
		config.Agent, err = agent.NewService(context.Background(), config.Database, nil)
		if err != nil {
			return nil, err
		}
		ownedAgent = true
	}
	roots := canonicalRoots(config.FSRoots)
	if len(roots) == 0 {
		roots = defaultFSRoots()
	}
	return &Server{
		database: config.Database, token: config.Token, port: config.Port,
		fsRoots: roots, sseMaxEvents: config.SSEMaxEvents,
		logger: config.Logger, picker: config.Picker, agent: config.Agent, ownsAgent: ownedAgent,
	}, nil
}

func (server *Server) Close() {
	if server.ownsAgent {
		server.agent.Close()
	}
}

func (server *Server) Handler() http.Handler {
	router := chi.NewRouter()
	router.Use(server.securityMiddleware)
	router.Get("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	return HandlerFromMuxWithBaseURL(server, router, "")
}

func GenerateToken() string {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func newID(prefix string) string {
	data := make([]byte, 12)
	if _, err := rand.Read(data); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(data)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func decodeJSON(request *http.Request, destination any) error {
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("请求体只能包含一个 JSON 值")
		}
		return err
	}
	return nil
}

func writeBadRequest(writer http.ResponseWriter, reason string) {
	writeJSON(writer, http.StatusBadRequest, map[string]any{
		"detail": map[string]string{"reason": reason},
	})
}

func writeNotFound(writer http.ResponseWriter, reason string) {
	writeJSON(writer, http.StatusNotFound, map[string]any{
		"detail": map[string]string{"reason": reason},
	})
}

func canonicalRoots(roots []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(roots))
	for _, root := range roots {
		absolute, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		absolute = filepath.Clean(absolute)
		if evaluated, err := filepath.EvalSymlinks(absolute); err == nil {
			absolute = evaluated
		}
		if _, ok := seen[absolute]; ok {
			continue
		}
		seen[absolute] = struct{}{}
		result = append(result, absolute)
	}
	sort.Strings(result)
	return result
}
