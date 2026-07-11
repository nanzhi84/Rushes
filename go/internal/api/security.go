package api

import (
	"crypto/subtle"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (server *Server) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.URL.Path, "/api/") {
			next.ServeHTTP(writer, request)
			return
		}
		expectedHost := "127.0.0.1:" + portString(server.port)
		if strings.ToLower(request.Host) != expectedHost {
			server.securityRefusal(writer, request, http.StatusForbidden, "host_mismatch")
			return
		}
		if origin := request.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || parsed.Scheme != "http" || parsed.Host != expectedHost || parsed.Path != "" {
				server.securityRefusal(writer, request, http.StatusForbidden, "origin_mismatch")
				return
			}
		}
		provided, present := requestToken(request)
		if !present {
			server.securityRefusal(writer, request, http.StatusUnauthorized, "missing_token")
			return
		}
		if subtle.ConstantTimeCompare([]byte(provided), []byte(server.token)) != 1 {
			server.securityRefusal(writer, request, http.StatusUnauthorized, "bad_token")
			return
		}
		if isMutation(request.Method) {
			mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
			if err != nil || strings.ToLower(mediaType) != "application/json" {
				server.securityRefusal(writer, request, http.StatusUnsupportedMediaType, "bad_content_type")
				return
			}
		}
		next.ServeHTTP(writer, request)
	})
}

func requestToken(request *http.Request) (string, bool) {
	if authorization := request.Header.Get("Authorization"); authorization != "" {
		parts := strings.SplitN(authorization, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
			return "", true
		}
		return parts[1], true
	}
	if allowsQueryToken(request) {
		if token := request.URL.Query().Get("token"); token != "" {
			return token, true
		}
	}
	return "", false
}

func allowsQueryToken(request *http.Request) bool {
	path := request.URL.Path
	if path == "/api/events" || strings.HasSuffix(path, "/events") || strings.HasSuffix(path, "/turn-stream") {
		return true
	}
	return (request.Method == http.MethodGet || request.Method == http.MethodHead) &&
		strings.HasPrefix(path, "/api/media/")
}

func isMutation(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func (server *Server) securityRefusal(
	writer http.ResponseWriter,
	request *http.Request,
	status int,
	reason string,
) {
	server.logger.Warn("API 安全基线拒绝", "reason", reason, "route", request.URL.Path)
	writeJSON(writer, status, map[string]string{"error": "SecurityRefusal", "reason": reason})
}

func portString(port int) string {
	// strconv.Itoa 单独封装，便于安全中间件测试不依赖格式化模板。
	return strconv.Itoa(port)
}
