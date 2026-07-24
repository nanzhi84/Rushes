package agent

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// concurrencyProbe 记录并发峰值:每个工具进入时 inFlight+1 并抬高 maxSeen,退出时 -1。
// maxSeen 是并发的确定性证据,不受调度抖动影响,补足 wall-clock 断言。
type concurrencyProbe struct {
	inFlight atomic.Int32
	maxSeen  atomic.Int32
}

func (probe *concurrencyProbe) tool(name string, delay time.Duration) tool.BaseTool {
	impl, err := utils.InferTool(name, name, func(_ context.Context, _ struct{}) (string, error) {
		current := probe.inFlight.Add(1)
		for {
			seen := probe.maxSeen.Load()
			if current <= seen || probe.maxSeen.CompareAndSwap(seen, current) {
				break
			}
		}
		time.Sleep(delay)
		probe.inFlight.Add(-1)
		return name, nil
	})
	if err != nil {
		panic(err)
	}
	return impl
}

func routerMessage(names ...string) *schema.Message {
	calls := make([]schema.ToolCall, len(names))
	for index, name := range names {
		calls[index] = schema.ToolCall{
			ID:       "call_" + name,
			Function: schema.FunctionCall{Name: name, Arguments: "{}"},
		}
	}
	return &schema.Message{Role: schema.Assistant, ToolCalls: calls}
}

func routerMessageWithArguments(calls ...schema.FunctionCall) *schema.Message {
	toolCalls := make([]schema.ToolCall, len(calls))
	for index, call := range calls {
		toolCalls[index] = schema.ToolCall{ID: "call_" + call.Name, Function: call}
	}
	return &schema.Message{Role: schema.Assistant, ToolCalls: toolCalls}
}

func newRouterForTest(t *testing.T, effect map[string]rushestools.Effect, tools ...tool.BaseTool) *toolRouter {
	t.Helper()
	specs := make(map[string]rushestools.Spec, len(effect))
	for name, value := range effect {
		family := rushestools.FamilyEdit
		if value == rushestools.EffectReadOnly {
			family = rushestools.FamilyRead
		}
		specs[name] = rushestools.Spec{Name: name, Family: family, Effect: value}
	}
	router, err := newToolRouter(t.Context(), compose.ToolsNodeConfig{Tools: tools},
		func(name string) (rushestools.Spec, bool) { value, ok := specs[name]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func TestToolRouterRunsReadOnlyMessagesInParallel(t *testing.T) {
	t.Parallel()
	probe := &concurrencyProbe{}
	const delay = 80 * time.Millisecond
	router := newRouterForTest(t,
		map[string]rushestools.Effect{
			"read.a": rushestools.EffectReadOnly,
			"read.b": rushestools.EffectReadOnly,
			"read.c": rushestools.EffectReadOnly,
		},
		probe.tool("read.a", delay), probe.tool("read.b", delay), probe.tool("read.c", delay))

	start := time.Now()
	results, err := router.Invoke(t.Context(), routerMessage("read.a", "read.b", "read.c"))
	elapsed := time.Since(start)
	if err != nil || len(results) != 3 {
		t.Fatalf("results=%#v err=%v", results, err)
	}
	if results[0].ToolCallID != "call_read.a" || results[1].ToolCallID != "call_read.b" ||
		results[2].ToolCallID != "call_read.c" {
		t.Fatalf("并发结果未按原下标保序: %s,%s,%s",
			results[0].ToolCallID, results[1].ToolCallID, results[2].ToolCallID)
	}
	if probe.maxSeen.Load() != 3 {
		t.Fatalf("只读消息未并发执行: 并发峰值=%d", probe.maxSeen.Load())
	}
	if elapsed >= 3*delay {
		t.Fatalf("wall-clock 未低于串行和: elapsed=%v serial≈%v", elapsed, 3*delay)
	}
}

func TestToolRouterSerializesMessagesWithWrites(t *testing.T) {
	t.Parallel()
	probe := &concurrencyProbe{}
	const delay = 40 * time.Millisecond
	router := newRouterForTest(t,
		map[string]rushestools.Effect{"read.a": rushestools.EffectReadOnly, "write.b": rushestools.EffectReversible},
		probe.tool("read.a", delay), probe.tool("write.b", delay))

	results, err := router.Invoke(t.Context(), routerMessage("read.a", "write.b"))
	if err != nil || len(results) != 2 {
		t.Fatalf("results=%#v err=%v", results, err)
	}
	if results[0].ToolCallID != "call_read.a" || results[1].ToolCallID != "call_write.b" {
		t.Fatal("串行结果未保序")
	}
	if probe.maxSeen.Load() != 1 {
		t.Fatalf("含写消息不得并发: 并发峰值=%d", probe.maxSeen.Load())
	}
}

func TestToolRouterRunsIndependentDetectorsInParallel(t *testing.T) {
	t.Parallel()
	probe := &concurrencyProbe{}
	const delay = 80 * time.Millisecond
	tools := []tool.BaseTool{
		probe.tool("media.detect_shots", delay),
		probe.tool("speech.transcribe", delay),
	}
	specs := map[string]rushestools.Spec{
		"media.detect_shots": {
			Name: "media.detect_shots", Family: rushestools.FamilyDetect,
			Effect: rushestools.EffectReversible,
		},
		"speech.transcribe": {
			Name: "speech.transcribe", Family: rushestools.FamilyDetect,
			Effect: rushestools.EffectReversible,
		},
	}
	router, err := newToolRouter(
		t.Context(),
		compose.ToolsNodeConfig{Tools: tools},
		func(name string) (rushestools.Spec, bool) { spec, ok := specs[name]; return spec, ok },
	)
	if err != nil {
		t.Fatal(err)
	}
	message := routerMessageWithArguments(
		schema.FunctionCall{Name: "media.detect_shots", Arguments: `{"asset_id":"asset_a"}`},
		schema.FunctionCall{Name: "media.detect_shots", Arguments: `{"asset_id":"asset_b"}`},
		schema.FunctionCall{Name: "speech.transcribe", Arguments: `{"asset_id":"asset_a"}`},
	)
	results, err := router.Invoke(t.Context(), message)
	if err != nil || len(results) != 3 {
		t.Fatalf("results=%#v err=%v", results, err)
	}
	if probe.maxSeen.Load() != 3 {
		t.Fatalf("独立 detector 未并行执行: 并发峰值=%d", probe.maxSeen.Load())
	}
}

type blockingSpeechRecognizer struct {
	inFlight atomic.Int32
	maxSeen  atomic.Int32
	entered  atomic.Int32
	ready    chan struct{}
	once     sync.Once
}

type serializedSpeechRecognizer struct {
	inFlight atomic.Int32
	maxSeen  atomic.Int32
	calls    atomic.Int32
}

func (recognizer *serializedSpeechRecognizer) Recognize(
	_ context.Context, _ contracts.SpeechRecognitionRequest,
) (contracts.SpeechRecognitionResult, error) {
	current := recognizer.inFlight.Add(1)
	defer recognizer.inFlight.Add(-1)
	recognizer.calls.Add(1)
	for {
		seen := recognizer.maxSeen.Load()
		if current <= seen || recognizer.maxSeen.CompareAndSwap(seen, current) {
			break
		}
	}
	time.Sleep(100 * time.Millisecond)
	return contracts.SpeechRecognitionResult{
		Text: "跨草稿共享口播", Language: "zh", ProviderID: "serialized-test",
		Segments: []contracts.SpeechRecognitionSegment{{
			Text: "跨草稿共享口播", BeginMilliseconds: 0, EndMilliseconds: 800,
		}},
	}, nil
}

func (recognizer *blockingSpeechRecognizer) Recognize(
	ctx context.Context, _ contracts.SpeechRecognitionRequest,
) (contracts.SpeechRecognitionResult, error) {
	current := recognizer.inFlight.Add(1)
	defer recognizer.inFlight.Add(-1)
	for {
		seen := recognizer.maxSeen.Load()
		if current <= seen || recognizer.maxSeen.CompareAndSwap(seen, current) {
			break
		}
	}
	if recognizer.entered.Add(1) == 2 {
		recognizer.once.Do(func() { close(recognizer.ready) })
	}
	select {
	case <-recognizer.ready:
	case <-ctx.Done():
		return contracts.SpeechRecognitionResult{}, ctx.Err()
	}
	return contracts.SpeechRecognitionResult{
		Text: "并发口播", Language: "zh", ProviderID: "blocking-test",
		Segments: []contracts.SpeechRecognitionSegment{{
			Text: "并发口播", BeginMilliseconds: 0, EndMilliseconds: 800,
		}},
	}, nil
}

// 覆盖生产调用链：Registry 工具解码 -> Service.ExecuteTool -> agentexec.Executor。
// 仅测裸 ToolsNode 会漏掉 Service 的回合执行屏障。
func TestServiceRegistryAllowsIndependentDetectorsInParallel(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_detector_barrier"
	agenttest.CreateAgentDraft(t, database, draftID)
	for _, assetID := range []string{"asset_a", "asset_b"} {
		audioPath := filepath.Join(database.Paths.Temporary, assetID+".wav")
		if _, err := media.RunCommand(
			t.Context(), "ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
			"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=16000:duration=1",
			"-c:a", "pcm_s16le", audioPath,
		); err != nil {
			t.Fatal(err)
		}
		agenttest.InsertSpeechFixtureAsset(t, database, draftID, assetID, audioPath)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	recognizer := &blockingSpeechRecognizer{ready: make(chan struct{})}
	service.SetSpeechRecognizer(recognizer)
	router, err := newToolRouter(
		t.Context(),
		compose.ToolsNodeConfig{Tools: service.Tools().EinoTools(true, false)},
		service.Tools().Spec,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx = rushestools.WithDraftID(ctx, draftID)
	ctx = agentexec.WithTurnInteractionState(ctx, agentexec.NewTurnInteractionState())
	results, err := router.Invoke(ctx, routerMessageWithArguments(
		schema.FunctionCall{Name: "speech.transcribe", Arguments: `{"asset_id":"asset_a"}`},
		schema.FunctionCall{Name: "speech.transcribe", Arguments: `{"asset_id":"asset_b"}`},
	))
	if err != nil || len(results) != 2 {
		t.Fatalf("results=%#v err=%v", results, err)
	}
	if recognizer.maxSeen.Load() != 2 {
		t.Fatalf("Service 执行屏障仍串行化独立 detector: 并发峰值=%d", recognizer.maxSeen.Load())
	}
}

func TestServiceSerializesSameAssetDetectorAcrossTurnsAndDrafts(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const (
		firstDraft  = "draft_shared_detector_a"
		secondDraft = "draft_shared_detector_b"
		assetID     = "asset_shared_detector"
	)
	agenttest.CreateAgentDraft(t, database, firstDraft)
	agenttest.CreateAgentDraft(t, database, secondDraft)
	audioPath := filepath.Join(database.Paths.Temporary, "shared-detector.wav")
	if _, err := media.RunCommand(
		t.Context(), "ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=16000:duration=1",
		"-c:a", "pcm_s16le", audioPath,
	); err != nil {
		t.Fatal(err)
	}
	agenttest.InsertSpeechFixtureAsset(t, database, firstDraft, assetID, audioPath)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO draft_asset_links(draft_id,asset_id,rel_dir,linked_at)
		VALUES(?, ?, 'Aroll', ?)`, secondDraft, assetID, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	recognizer := &serializedSpeechRecognizer{}
	service.SetSpeechRecognizer(recognizer)

	start := make(chan struct{})
	results := make(chan rushestools.SpeechTranscribeResult, 2)
	errors := make(chan error, 2)
	for _, draftID := range []string{firstDraft, secondDraft} {
		go func() {
			<-start
			ctx := rushestools.WithDraftID(t.Context(), draftID)
			ctx = agentexec.WithTurnInteractionState(
				ctx,
				agentexec.NewTurnInteractionState(service.indexedResources),
			)
			raw, executeErr := service.ExecuteTool(
				ctx,
				"speech.transcribe",
				rushestools.SpeechTranscribeInput{AssetID: assetID, Language: "zh"},
			)
			if executeErr != nil {
				errors <- executeErr
				return
			}
			results <- raw.(rushestools.SpeechTranscribeResult)
		}()
	}
	close(start)
	collected := make([]rushestools.SpeechTranscribeResult, 0, 2)
	for len(collected) < 2 {
		select {
		case executeErr := <-errors:
			t.Fatal(executeErr)
		case result := <-results:
			collected = append(collected, result)
		case <-time.After(5 * time.Second):
			t.Fatal("跨 turn 同素材 detector 未完成")
		}
	}
	cacheHits := 0
	for _, result := range collected {
		if result.CacheHit {
			cacheHits++
		}
	}
	var transcripts int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM transcripts WHERE asset_id=?", assetID,
	).Scan(&transcripts); err != nil {
		t.Fatal(err)
	}
	if recognizer.calls.Load() != 1 || recognizer.maxSeen.Load() != 1 ||
		cacheHits != 1 || transcripts != 1 {
		t.Fatalf(
			"calls=%d max=%d cache_hits=%d transcripts=%d results=%#v",
			recognizer.calls.Load(), recognizer.maxSeen.Load(), cacheHits, transcripts, collected,
		)
	}
}

func TestToolRouterSerializesDuplicateDetectorResource(t *testing.T) {
	t.Parallel()
	probe := &concurrencyProbe{}
	const name = "speech.transcribe"
	spec := rushestools.Spec{
		Name: name, Family: rushestools.FamilyDetect, Effect: rushestools.EffectReversible,
	}
	router, err := newToolRouter(
		t.Context(),
		compose.ToolsNodeConfig{Tools: []tool.BaseTool{probe.tool(name, 30*time.Millisecond)}},
		func(toolName string) (rushestools.Spec, bool) { return spec, toolName == name },
	)
	if err != nil {
		t.Fatal(err)
	}
	message := routerMessageWithArguments(
		schema.FunctionCall{Name: name, Arguments: `{"asset_id":"same"}`},
		schema.FunctionCall{Name: name, Arguments: `{"asset_id":"same"}`},
	)
	if _, err := router.Invoke(t.Context(), message); err != nil {
		t.Fatal(err)
	}
	if probe.maxSeen.Load() != 1 {
		t.Fatalf("同资源 detector 必须串行: 并发峰值=%d", probe.maxSeen.Load())
	}
}

func TestToolRouterSerializesDetectorAndDependentReadOnSameResource(t *testing.T) {
	t.Parallel()
	const delay = 30 * time.Millisecond
	specs := map[string]rushestools.Spec{
		"media.detect_shots": {
			Name: "media.detect_shots", Family: rushestools.FamilyDetect,
			Effect: rushestools.EffectReversible,
		},
		"speech.transcribe": {
			Name: "speech.transcribe", Family: rushestools.FamilyDetect,
			Effect: rushestools.EffectReversible,
		},
		"shot.search": {
			Name: "shot.search", Family: rushestools.FamilyRead,
			Effect: rushestools.EffectReadOnly,
		},
		"speech.search": {
			Name: "speech.search", Family: rushestools.FamilyRead,
			Effect: rushestools.EffectReadOnly,
		},
		"timeline.check": {
			Name: "timeline.check", Family: rushestools.FamilyRead,
			Effect: rushestools.EffectReadOnly,
		},
		"preview.check": {
			Name: "preview.check", Family: rushestools.FamilyRead,
			Effect: rushestools.EffectReadOnly,
		},
	}
	tests := []struct {
		name     string
		calls    []schema.FunctionCall
		parallel bool
	}{
		{
			name: "speech_same_asset",
			calls: []schema.FunctionCall{
				{Name: "speech.transcribe", Arguments: `{"asset_id":"asset_a"}`},
				{Name: "speech.search", Arguments: `{"asset_id":"asset_a"}`},
			},
		},
		{
			name: "speech_different_assets",
			calls: []schema.FunctionCall{
				{Name: "speech.transcribe", Arguments: `{"asset_id":"asset_a"}`},
				{Name: "speech.search", Arguments: `{"asset_id":"asset_b"}`},
			},
			parallel: true,
		},
		{
			name: "shots_same_asset",
			calls: []schema.FunctionCall{
				{Name: "media.detect_shots", Arguments: `{"asset_id":"asset_a"}`},
				{Name: "shot.search", Arguments: `{"asset_ids":["asset_a"]}`},
			},
		},
		{
			name: "shots_different_assets",
			calls: []schema.FunctionCall{
				{Name: "media.detect_shots", Arguments: `{"asset_id":"asset_a"}`},
				{Name: "shot.search", Arguments: `{"asset_ids":["asset_b"]}`},
			},
			parallel: true,
		},
		{
			name: "shots_unrestricted_search",
			calls: []schema.FunctionCall{
				{Name: "media.detect_shots", Arguments: `{"asset_id":"asset_a"}`},
				{Name: "shot.search", Arguments: `{}`},
			},
		},
		{
			name: "shot_search_reads_same_asset_transcript",
			calls: []schema.FunctionCall{
				{Name: "speech.transcribe", Arguments: `{"asset_id":"asset_a"}`},
				{Name: "shot.search", Arguments: `{"asset_ids":["asset_a"]}`},
			},
		},
		{
			name: "shot_search_reads_different_asset_transcript",
			calls: []schema.FunctionCall{
				{Name: "speech.transcribe", Arguments: `{"asset_id":"asset_a"}`},
				{Name: "shot.search", Arguments: `{"asset_ids":["asset_b"]}`},
			},
			parallel: true,
		},
		{
			name: "timeline_check_reads_transcripts",
			calls: []schema.FunctionCall{
				{Name: "speech.transcribe", Arguments: `{"asset_id":"asset_a"}`},
				{Name: "timeline.check", Arguments: `{}`},
			},
		},
		{
			name: "visual_preview_reads_transcripts",
			calls: []schema.FunctionCall{
				{Name: "speech.transcribe", Arguments: `{"asset_id":"asset_a"}`},
				{Name: "preview.check", Arguments: `{"preview_id":"preview","check":"visual"}`},
			},
		},
		{
			name: "non_visual_preview_is_independent",
			calls: []schema.FunctionCall{
				{Name: "speech.transcribe", Arguments: `{"asset_id":"asset_a"}`},
				{Name: "preview.check", Arguments: `{"preview_id":"preview","check":"decode"}`},
			},
			parallel: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probe := &concurrencyProbe{}
			tools := make([]tool.BaseTool, 0, len(test.calls))
			for _, call := range test.calls {
				tools = append(tools, probe.tool(call.Name, delay))
			}
			router, err := newToolRouter(
				t.Context(), compose.ToolsNodeConfig{Tools: tools},
				func(name string) (rushestools.Spec, bool) { spec, ok := specs[name]; return spec, ok },
			)
			if err != nil {
				t.Fatal(err)
			}
			message := routerMessageWithArguments(test.calls...)
			if got := router.canRunParallel(message); got != test.parallel {
				t.Fatalf("canRunParallel=%v want=%v", got, test.parallel)
			}
			if _, err := router.Invoke(t.Context(), message); err != nil {
				t.Fatal(err)
			}
			wantPeak := int32(1)
			if test.parallel {
				wantPeak = 2
			}
			if probe.maxSeen.Load() != wantPeak {
				t.Fatalf("并发峰值=%d want=%d", probe.maxSeen.Load(), wantPeak)
			}
		})
	}
}

func TestServiceIndexedFootprintsLockEveryTranscriptConsumer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		readName    string
		readInput   any
		shouldBlock bool
	}{
		{
			name: "shot_search_same_asset", readName: "shot.search",
			readInput:   rushestools.ShotSearchInput{AssetIDs: []string{"asset_a"}},
			shouldBlock: true,
		},
		{
			name: "shot_search_different_asset", readName: "shot.search",
			readInput: rushestools.ShotSearchInput{AssetIDs: []string{"asset_b"}},
		},
		{
			name: "timeline_check", readName: "timeline.check",
			readInput: rushestools.TimelineCheckInput{}, shouldBlock: true,
		},
		{
			name: "visual_preview", readName: "preview.check",
			readInput: rushestools.PreviewCheckInput{
				PreviewID: "preview", Check: "visual",
			},
			shouldBlock: true,
		},
		{
			name: "decode_preview", readName: "preview.check",
			readInput: rushestools.PreviewCheckInput{
				PreviewID: "preview", Check: "decode",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := agentexec.NewTurnInteractionState()
			ctx := agentexec.WithTurnInteractionState(t.Context(), state)
			service := &Service{}
			releaseWrite, _ := service.beginToolCall(
				ctx, "speech.transcribe",
				rushestools.SpeechTranscribeInput{AssetID: "asset_a"}, false,
			)
			acquired := make(chan func(), 1)
			go func() {
				releaseRead, _ := service.beginToolCall(
					ctx, test.readName, test.readInput, true,
				)
				acquired <- releaseRead
			}()
			if test.shouldBlock {
				select {
				case releaseRead := <-acquired:
					releaseRead()
					releaseWrite()
					t.Fatal("读取者绕过了同素材 speech 写锁")
				case <-time.After(30 * time.Millisecond):
				}
				releaseWrite()
				select {
				case releaseRead := <-acquired:
					releaseRead()
				case <-time.After(time.Second):
					t.Fatal("speech 写锁释放后读取者仍未继续")
				}
				return
			}
			select {
			case releaseRead := <-acquired:
				releaseRead()
			case <-time.After(time.Second):
				releaseWrite()
				t.Fatal("无关读取被 speech 写锁错误串行化")
			}
			releaseWrite()
		})
	}
}

func TestServiceAllResourceReadsShareBarrierButStillBlockDetector(t *testing.T) {
	t.Parallel()
	state := agentexec.NewTurnInteractionState()
	ctx := agentexec.WithTurnInteractionState(t.Context(), state)
	service := &Service{}
	releaseAllRead, _ := service.beginToolCall(
		ctx, "timeline.check", rushestools.TimelineCheckInput{}, true,
	)
	for _, test := range []struct {
		name  string
		input any
	}{
		{name: "speech.search", input: rushestools.SpeechSearchInput{}},
		{name: "speech.search", input: rushestools.SpeechSearchInput{AssetID: "asset_a"}},
	} {
		acquired := make(chan func(), 1)
		go func() {
			releaseRead, _ := service.beginToolCall(ctx, test.name, test.input, true)
			acquired <- releaseRead
		}()
		select {
		case releaseRead := <-acquired:
			releaseRead()
		case <-time.After(time.Second):
			releaseAllRead()
			t.Fatalf("全域纯读错误串行化了 %#v", test.input)
		}
	}

	acquiredWrite := make(chan func(), 1)
	go func() {
		releaseWrite, _ := service.beginToolCall(
			ctx, "speech.transcribe",
			rushestools.SpeechTranscribeInput{AssetID: "asset_a"}, false,
		)
		acquiredWrite <- releaseWrite
	}()
	select {
	case releaseWrite := <-acquiredWrite:
		releaseWrite()
		releaseAllRead()
		t.Fatal("detector 绕过了全域读取屏障")
	case <-time.After(30 * time.Millisecond):
	}
	releaseAllRead()
	select {
	case releaseWrite := <-acquiredWrite:
		releaseWrite()
	case <-time.After(time.Second):
		t.Fatal("全域读取释放后 detector 仍未继续")
	}
}

func TestToolRouterClassification(t *testing.T) {
	t.Parallel()
	effect := map[string]rushestools.Effect{
		"read.a": rushestools.EffectReadOnly, "read.b": rushestools.EffectReadOnly,
		"write.c": rushestools.EffectReversible,
	}
	router := newRouterForTest(t, effect,
		newRouterNoopTool("read.a"), newRouterNoopTool("read.b"), newRouterNoopTool("write.c"))
	cases := []struct {
		names    []string
		parallel bool
	}{
		{[]string{"read.a", "read.b"}, true},
		{[]string{"read.a", "write.c"}, false},
		{[]string{"write.c"}, false},
		{[]string{"unknown.x"}, false},
		{nil, false},
	}
	for _, test := range cases {
		if got := router.canRunParallel(routerMessage(test.names...)); got != test.parallel {
			t.Fatalf("canRunParallel(%v)=%v 期望 %v", test.names, got, test.parallel)
		}
	}
}

func newRouterNoopTool(name string) tool.BaseTool {
	impl, err := utils.InferTool(name, name, func(_ context.Context, _ struct{}) (string, error) {
		return name, nil
	})
	if err != nil {
		panic(err)
	}
	return impl
}
