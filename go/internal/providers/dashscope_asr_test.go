package providers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
)

func TestDashScopeASRUsesLocalAudioAndParsesEvidence(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/chat/completions" || request.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("request=%s auth=%q", request.URL.Path, request.Header.Get("Authorization"))
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(payload)
		if !strings.Contains(string(encoded), "data:audio/mpeg;base64,") ||
			!strings.Contains(string(encoded), `"language":"zh"`) {
			t.Fatalf("payload=%s", encoded)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"content":"这是口播。","annotations":[{"type":"audio_info","language":"zh","emotion":"neutral"}]}}]}`))
	}))
	defer server.Close()
	recognizer, err := NewDashScopeASR(DashScopeASRConfig{
		APIKey: "test-key", BaseURL: server.URL, Model: "qwen-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	recognizer.client = server.Client()
	path := filepath.Join(t.TempDir(), "speech.mp3")
	if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := recognizer.Recognize(t.Context(), contracts.SpeechRecognitionRequest{
		AudioPath: path, Language: "zh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "这是口播。" || result.Language != "zh" ||
		result.Emotion != "neutral" || result.ProviderID != "qwen-test" {
		t.Fatalf("result=%#v", result)
	}
}

func TestDashScopeFunASRUsesGenerationEndpointAndParsesTimestamps(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != dashScopeGenerationPath ||
			request.Header.Get("X-DashScope-SSE") != "disable" {
			t.Fatalf("path=%s sse=%q", request.URL.Path, request.Header.Get("X-DashScope-SSE"))
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(payload)
		body := string(encoded)
		parameters, _ := payload["parameters"].(map[string]any)
		if !strings.Contains(body, `"model":"fun-asr-flash-2026-06-15"`) ||
			!strings.Contains(body, `"input_audio"`) ||
			!strings.Contains(body, `"format":"mp3"`) ||
			parameters["sample_rate"] != "16000" ||
			strings.Contains(body, `"messages":[`) && payload["messages"] != nil {
			t.Fatalf("payload=%s", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"output":{"text":"这是一段口播。下一句。","sentence":{
				"begin_time":120,"end_time":1880,"text":"这是一段口播。下一句。",
				"words":[
					{"begin_time":120,"end_time":900,"text":"这是一段口播","punctuation":"。"},
					{"begin_time":1100,"end_time":1880,"text":"下一句","punctuation":"。"}
				]
			}},"request_id":"request-1"
		}`))
	}))
	defer server.Close()
	recognizer, err := NewDashScopeASR(DashScopeASRConfig{
		APIKey: "test-key", BaseURL: server.URL, Model: "fun-asr-flash-2026-06-15",
	})
	if err != nil {
		t.Fatal(err)
	}
	recognizer.client = server.Client()
	path := filepath.Join(t.TempDir(), "speech.mp3")
	if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := recognizer.Recognize(t.Context(), contracts.SpeechRecognitionRequest{
		AudioPath: path, Language: "zh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "这是一段口播。下一句。" || result.Language != "zh" ||
		result.ProviderID != DefaultASRModel || len(result.Segments) != 1 ||
		result.Segments[0].BeginMilliseconds != 120 || len(result.Segments[0].Words) != 2 {
		t.Fatalf("result=%#v", result)
	}
}

func TestDashScopeFunASRMarksNoWordsAsSkippable(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"code":"BadRequest","message":"ASR_RESPONSE_HAVE_NO_WORDS"}`))
	}))
	defer server.Close()
	recognizer, err := NewDashScopeASR(DashScopeASRConfig{
		APIKey: "key", BaseURL: server.URL, Model: DefaultASRModel,
	})
	if err != nil {
		t.Fatal(err)
	}
	recognizer.client = server.Client()
	path := filepath.Join(t.TempDir(), "silence.mp3")
	if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = recognizer.Recognize(t.Context(), contracts.SpeechRecognitionRequest{AudioPath: path})
	if !errors.Is(err, contracts.ErrSpeechNoWords) {
		t.Fatalf("err=%v", err)
	}
}

func TestDashScopeFunASRParsesNestedSnapshotPayload(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{
			"output":{"output":{"text":"完整文本。","sentence":{
				"begin_time":100,"end_time":900,"text":"完整文本。"
			}}}
		}`))
	}))
	defer server.Close()
	recognizer, err := NewDashScopeASR(DashScopeASRConfig{APIKey: "key", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	recognizer.client = server.Client()
	path := filepath.Join(t.TempDir(), "speech.wav")
	if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := recognizer.Recognize(t.Context(), contracts.SpeechRecognitionRequest{AudioPath: path})
	if err != nil || result.Text != "完整文本。" || len(result.Segments) != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestDashScopeASRModelAndPayloadHelpers(t *testing.T) {
	t.Parallel()
	qwenRecognizer, err := NewDashScopeASR(DashScopeASRConfig{
		APIKey: "key", Model: "qwen3-asr-flash",
	})
	if err != nil || qwenRecognizer.baseURL != DefaultDashScopeBaseURL {
		t.Fatalf("recognizer=%#v err=%v", qwenRecognizer, err)
	}
	funRecognizer, err := NewDashScopeASR(DashScopeASRConfig{APIKey: "key"})
	if err != nil || funRecognizer.funASREndpoint() != DefaultFunASRBaseURL {
		t.Fatalf("recognizer=%#v endpoint=%q err=%v", funRecognizer, funRecognizer.funASREndpoint(), err)
	}
	if audioFormat("speech.OPUS") != "opus" ||
		!strings.HasPrefix(audioDataURI("speech.opus", []byte("audio")), "data:audio/ogg;base64,") {
		t.Fatal("opus MIME 或格式映射错误")
	}
	longError := strings.Repeat("x", 350)
	if got := providerErrorMessage([]byte(longError)); utf8.RuneCountInString(got) != 301 ||
		!strings.HasSuffix(got, "…") {
		t.Fatalf("truncated=%q", got)
	}
}

func TestDashScopeFunASRFallsBackToSentenceText(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{
			"output":{"sentence":{"begin_time":100,"end_time":500,"text":"仅句子文本。"}}
		}`))
	}))
	defer server.Close()
	recognizer, err := NewDashScopeASR(DashScopeASRConfig{APIKey: "key", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	recognizer.client = server.Client()
	path := filepath.Join(t.TempDir(), "speech.mp3")
	if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := recognizer.Recognize(t.Context(), contracts.SpeechRecognitionRequest{AudioPath: path})
	if err != nil || result.Text != "仅句子文本。" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestDashScopeQwenASRRejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`not-json`))
	}))
	defer server.Close()
	recognizer, err := NewDashScopeASR(DashScopeASRConfig{
		APIKey: "key", BaseURL: server.URL, Model: "qwen3-asr-flash",
	})
	if err != nil {
		t.Fatal(err)
	}
	recognizer.client = server.Client()
	path := filepath.Join(t.TempDir(), "speech.mp3")
	if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := recognizer.Recognize(t.Context(), contracts.SpeechRecognitionRequest{AudioPath: path}); err == nil {
		t.Fatal("Qwen 非法 JSON 应失败")
	}
}

func TestDashScopeASRRejectsMissingCredentialsAndProviderErrors(t *testing.T) {
	t.Parallel()
	if _, err := NewDashScopeASR(DashScopeASRConfig{}); err == nil {
		t.Fatal("空密钥应失败")
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer server.Close()
	recognizer, err := NewDashScopeASR(DashScopeASRConfig{APIKey: "key", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	recognizer.client = server.Client()
	path := filepath.Join(t.TempDir(), "speech.mp3")
	if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := recognizer.Recognize(t.Context(), contracts.SpeechRecognitionRequest{AudioPath: path}); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err=%v", err)
	}
}

func TestDashScopeASRRejectsInvalidLocalAndMalformedProviderPayloads(t *testing.T) {
	t.Parallel()
	recognizer, err := NewDashScopeASR(DashScopeASRConfig{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recognizer.Recognize(t.Context(), contracts.SpeechRecognitionRequest{
		AudioPath: filepath.Join(t.TempDir(), "missing.mp3"),
	}); err == nil {
		t.Fatal("不存在的本地音频应失败")
	}
	empty := filepath.Join(t.TempDir(), "empty.mp3")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := recognizer.Recognize(t.Context(), contracts.SpeechRecognitionRequest{AudioPath: empty}); err == nil {
		t.Fatal("空音频应失败")
	}
	large := filepath.Join(t.TempDir(), "large.mp3")
	if err := os.WriteFile(large, []byte{1}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(large, 10*1024*1024+1); err != nil {
		t.Fatal(err)
	}
	if _, err := recognizer.Recognize(t.Context(), contracts.SpeechRecognitionRequest{AudioPath: large}); err == nil {
		t.Fatal("超限音频应失败")
	}

	for _, response := range []string{`not-json`, `{"choices":[]}`, `{"choices":[{"message":{"content":""}}]}`} {
		response := response
		t.Run(response, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(response))
			}))
			defer server.Close()
			local, err := NewDashScopeASR(DashScopeASRConfig{APIKey: "key", BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			local.client = server.Client()
			path := filepath.Join(t.TempDir(), "speech.mp3")
			if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := local.Recognize(t.Context(), contracts.SpeechRecognitionRequest{AudioPath: path}); err == nil {
				t.Fatal("无效 provider payload 应失败")
			}
		})
	}
	if got := providerErrorMessage([]byte(`{"message":"plain message"}`)); got != "plain message" {
		t.Fatalf("message=%q", got)
	}
	if got := providerErrorMessage([]byte("raw provider failure")); got != "raw provider failure" {
		t.Fatalf("raw=%q", got)
	}
}
