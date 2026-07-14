package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
)

const (
	DefaultASRModel         = "fun-asr-flash-2026-06-15"
	DefaultFunASRBaseURL    = "https://dashscope.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation"
	maxASRInputBytes        = 10 * 1024 * 1024
	dashScopeGenerationPath = "/api/v1/services/aigc/multimodal-generation/generation"
)

type DashScopeASRConfig struct {
	APIKey  string
	BaseURL string
	Model   string
	Timeout time.Duration
}

type DashScopeASR struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
}

func NewDashScopeASR(config DashScopeASRConfig) (*DashScopeASR, error) {
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("ASR 缺少 DashScope API 密钥")
	}
	if strings.TrimSpace(config.Model) == "" {
		config.Model = DefaultASRModel
	}
	if strings.TrimSpace(config.BaseURL) == "" {
		if isFunASRModel(config.Model) {
			config.BaseURL = DefaultFunASRBaseURL
		} else {
			config.BaseURL = DefaultDashScopeBaseURL
		}
	}
	if config.Timeout <= 0 {
		config.Timeout = 90 * time.Second
	}
	return &DashScopeASR{
		apiKey:  strings.TrimSpace(config.APIKey),
		baseURL: strings.TrimRight(strings.TrimSpace(config.BaseURL), "/"),
		model:   strings.TrimSpace(config.Model),
		client:  NewIPv4Client(config.Timeout),
	}, nil
}

func (recognizer *DashScopeASR) Recognize(
	ctx context.Context,
	request contracts.SpeechRecognitionRequest,
) (contracts.SpeechRecognitionResult, error) {
	data, err := os.ReadFile(request.AudioPath)
	if err != nil {
		return contracts.SpeechRecognitionResult{}, err
	}
	if len(data) == 0 {
		return contracts.SpeechRecognitionResult{}, errors.New("ASR 音频片段为空")
	}
	// Fun-ASR-Flash 与 Qwen3-ASR-Flash 的 Base64 请求都限制在 10 MB 内。
	// 本地媒体层默认以 64 kbps、最长 25 秒切块，正常远低于此上限。
	if len(data) > maxASRInputBytes {
		return contracts.SpeechRecognitionResult{}, errors.New("ASR 音频片段超过 10 MB")
	}
	dataURI := audioDataURI(request.AudioPath, data)
	if isFunASRModel(recognizer.model) {
		return recognizer.recognizeFunASR(ctx, request, dataURI)
	}
	return recognizer.recognizeQwenASR(ctx, request, dataURI)
}

func (recognizer *DashScopeASR) recognizeFunASR(
	ctx context.Context,
	request contracts.SpeechRecognitionRequest,
	dataURI string,
) (contracts.SpeechRecognitionResult, error) {
	payload := map[string]any{
		"model": recognizer.model,
		"input": map[string]any{"messages": []map[string]any{{
			"role": "user", "content": []map[string]any{{
				"type": "input_audio", "input_audio": map[string]any{"data": dataURI},
			}},
		}}},
		"parameters": map[string]any{
			"format": audioFormat(request.AudioPath), "sample_rate": "16000",
		},
	}
	body, err := recognizer.postJSON(ctx, recognizer.funASREndpoint(), payload, map[string]string{
		"X-DashScope-SSE": "disable",
	})
	if err != nil {
		return contracts.SpeechRecognitionResult{}, err
	}
	var decoded funASRResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return contracts.SpeechRecognitionResult{}, err
	}
	output := decoded.Output
	if strings.TrimSpace(output.Text) == "" && strings.TrimSpace(output.Output.Text) != "" {
		output.Text = output.Output.Text
	}
	if strings.TrimSpace(output.Sentence.Text) == "" && strings.TrimSpace(output.Output.Sentence.Text) != "" {
		output.Sentence = output.Output.Sentence
	}
	if strings.TrimSpace(output.Text) == "" {
		output.Text = output.Sentence.Text
	}
	if strings.TrimSpace(output.Text) == "" {
		return contracts.SpeechRecognitionResult{}, contracts.ErrSpeechNoWords
	}
	result := contracts.SpeechRecognitionResult{
		Text: strings.TrimSpace(output.Text), Language: strings.TrimSpace(request.Language),
		ProviderID: recognizer.model,
	}
	if segment := output.Sentence.contractSegment(); segment.Text != "" {
		result.Segments = []contracts.SpeechRecognitionSegment{segment}
	}
	return result, nil
}

func (recognizer *DashScopeASR) recognizeQwenASR(
	ctx context.Context,
	request contracts.SpeechRecognitionRequest,
	dataURI string,
) (contracts.SpeechRecognitionResult, error) {
	payload := map[string]any{
		"model": recognizer.model,
		"messages": []map[string]any{{
			"role": "user", "content": []map[string]any{{
				"type": "input_audio", "input_audio": map[string]any{"data": dataURI},
			}},
		}},
		"stream": false, "asr_options": map[string]any{"enable_itn": true},
	}
	if language := strings.TrimSpace(request.Language); language != "" {
		payload["asr_options"].(map[string]any)["language"] = language
	}
	body, err := recognizer.postJSON(ctx, recognizer.baseURL+"/chat/completions", payload, nil)
	if err != nil {
		return contracts.SpeechRecognitionResult{}, err
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content     string `json:"content"`
				Annotations []struct {
					Type     string `json:"type"`
					Language string `json:"language"`
					Emotion  string `json:"emotion"`
				} `json:"annotations"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return contracts.SpeechRecognitionResult{}, err
	}
	if len(decoded.Choices) == 0 || strings.TrimSpace(decoded.Choices[0].Message.Content) == "" {
		return contracts.SpeechRecognitionResult{}, contracts.ErrSpeechNoWords
	}
	result := contracts.SpeechRecognitionResult{
		Text: strings.TrimSpace(decoded.Choices[0].Message.Content), ProviderID: recognizer.model,
	}
	for _, annotation := range decoded.Choices[0].Message.Annotations {
		if annotation.Type == "audio_info" {
			result.Language, result.Emotion = annotation.Language, annotation.Emotion
			break
		}
	}
	return result, nil
}

type funASRWord struct {
	Text        string `json:"text"`
	BeginTime   int    `json:"begin_time"`
	EndTime     int    `json:"end_time"`
	Punctuation string `json:"punctuation"`
}

type funASRSentence struct {
	Text      string       `json:"text"`
	BeginTime int          `json:"begin_time"`
	EndTime   int          `json:"end_time"`
	Words     []funASRWord `json:"words"`
}

type funASROutput struct {
	Text     string         `json:"text"`
	Sentence funASRSentence `json:"sentence"`
	Output   struct {
		Text     string         `json:"text"`
		Sentence funASRSentence `json:"sentence"`
	} `json:"output"`
}

type funASRResponse struct {
	Output funASROutput `json:"output"`
}

func (sentence funASRSentence) contractSegment() contracts.SpeechRecognitionSegment {
	segment := contracts.SpeechRecognitionSegment{
		Text:              strings.TrimSpace(sentence.Text),
		BeginMilliseconds: sentence.BeginTime, EndMilliseconds: sentence.EndTime,
	}
	for _, word := range sentence.Words {
		segment.Words = append(segment.Words, contracts.SpeechRecognitionWord{
			Text: word.Text, BeginMilliseconds: word.BeginTime,
			EndMilliseconds: word.EndTime, Punctuation: word.Punctuation,
		})
	}
	return segment
}

func (recognizer *DashScopeASR) postJSON(
	ctx context.Context, endpoint string, payload any, extraHeaders map[string]string,
) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+recognizer.apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	for key, value := range extraHeaders {
		httpRequest.Header.Set(key, value)
	}
	response, err := recognizer.client.Do(httpRequest)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := providerErrorMessage(body)
		if strings.Contains(strings.ToUpper(message), "ASR_RESPONSE_HAVE_NO_WORDS") {
			return nil, fmt.Errorf("%w: %s", contracts.ErrSpeechNoWords, message)
		}
		return nil, fmt.Errorf(
			"DashScope ASR 返回 HTTP %d: %s", response.StatusCode, message,
		)
	}
	return body, nil
}

func (recognizer *DashScopeASR) funASREndpoint() string {
	baseURL := strings.TrimRight(recognizer.baseURL, "/")
	if strings.HasSuffix(baseURL, dashScopeGenerationPath) {
		return baseURL
	}
	baseURL = strings.TrimSuffix(baseURL, "/compatible-mode/v1")
	return baseURL + dashScopeGenerationPath
}

func isFunASRModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "fun-asr-flash")
}

func audioFormat(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".wav":
		return "wav"
	case ".opus":
		return "opus"
	default:
		return "mp3"
	}
}

func audioDataURI(path string, data []byte) string {
	mimeType := "audio/mpeg"
	if audioFormat(path) == "wav" {
		mimeType = "audio/wav"
	} else if audioFormat(path) == "opus" {
		mimeType = "audio/ogg"
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func providerErrorMessage(body []byte) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &payload) == nil {
		if payload.Error.Message != "" {
			return payload.Error.Message
		}
		if payload.Message != "" {
			return payload.Message
		}
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	return text
}
