package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type editLeaseProviderSpy struct {
	database     *storage.DB
	draftID      string
	expectedLive []bool
	calls        int
	bound        [][]string
}

type leaseLossBlockingProvider struct {
	mu        sync.Mutex
	entered   chan struct{}
	enterOnce sync.Once
	calls     int
	bound     []string
}

func (provider *leaseLossBlockingProvider) WithTools(
	infos []*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Name)
	}
	provider.mu.Lock()
	provider.bound = names
	provider.mu.Unlock()
	return provider, nil
}

func (provider *leaseLossBlockingProvider) Generate(
	ctx context.Context,
	_ []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	provider.mu.Lock()
	provider.calls++
	provider.mu.Unlock()
	provider.enterOnce.Do(func() { close(provider.entered) })
	<-ctx.Done()
	return nil, context.Cause(ctx)
}

func (provider *leaseLossBlockingProvider) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := provider.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (provider *leaseLossBlockingProvider) snapshot() (int, []string) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.calls, append([]string(nil), provider.bound...)
}

func (spy *editLeaseProviderSpy) WithTools(
	infos []*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Name)
	}
	spy.bound = append(spy.bound, names)
	return spy, nil
}

func (spy *editLeaseProviderSpy) Generate(
	ctx context.Context,
	_ []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	if spy.calls >= len(spy.expectedLive) {
		return nil, fmt.Errorf("provider call %d 没有租约期望", spy.calls+1)
	}
	_, err := storage.GetLiveAgentEditLease(
		ctx, spy.database.Read(), spy.draftID, time.Now().UTC(),
	)
	live := err == nil
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("读取 provider 调用期 edit lease: %w", err)
	}
	if live != spy.expectedLive[spy.calls] {
		return nil, fmt.Errorf(
			"provider call %d live edit lease=%t want=%t",
			spy.calls+1, live, spy.expectedLive[spy.calls],
		)
	}
	spy.calls++
	return schema.AssistantMessage("ok", nil), nil
}

func (spy *editLeaseProviderSpy) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	response, err := spy.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{response}), nil
}

func TestTimelineEditLeaseSessionReleasesOnEveryExitShape(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	for _, exitShape := range []string{
		"terminal", "waiting_user", "cancel", "failure", "timeout", "panic", "lease_lost",
	} {
		t.Run(exitShape, func(t *testing.T) {
			draftID := "draft-lease-exit-" + exitShape
			agenttest.CreateAgentDraft(t, database, draftID)
			turnCtx, cancelTurn := context.WithCancelCause(t.Context())
			session := newTimelineEditLeaseSession(
				database, draftID, "turn-"+exitShape, cancelTurn,
			)
			if err := session.ensure(turnCtx); err != nil {
				t.Fatal(err)
			}
			if _, err := storage.GetLiveAgentEditLease(
				t.Context(), database.Read(), draftID, testNow(),
			); err != nil {
				t.Fatalf("lease was not acquired: %v", err)
			}
			func() {
				defer func() { _ = recover() }()
				defer session.close()
				switch exitShape {
				case "cancel":
					cancelTurn(context.Canceled)
				case "failure":
					_ = errors.New("provider failure")
				case "timeout":
					cancelTurn(context.DeadlineExceeded)
				case "panic":
					panic("fixture panic")
				case "lease_lost":
					session.markLost(storage.ErrAgentEditLeaseLost)
				case "waiting_user", "terminal":
					return
				default:
					panic(fmt.Sprintf("unknown exit shape %q", exitShape))
				}
			}()
			if _, err := storage.GetAgentEditLease(t.Context(), database.Read(), draftID); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("%s leaked edit lease: %v", exitShape, err)
			}
		})
	}
}

func TestTimelineEditLeaseHeartbeatRenewsAcrossRealInterval(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft-lease-real-heartbeat"
	agenttest.CreateAgentDraft(t, database, draftID)
	turnCtx, cancelTurn := context.WithCancelCause(t.Context())
	session := newTimelineEditLeaseSession(
		database, draftID, "turn-real-heartbeat", cancelTurn,
	)
	t.Cleanup(func() {
		session.close()
		cancelTurn(nil)
	})
	if err := session.ensure(turnCtx); err != nil {
		t.Fatal(err)
	}
	initial, err := storage.GetAgentEditLease(t.Context(), database.Read(), draftID)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(agentEditLeaseHeartbeatInterval + 5*time.Second)
	var renewed storage.AgentEditLease
	for time.Now().Before(deadline) {
		renewed, err = storage.GetAgentEditLease(t.Context(), database.Read(), draftID)
		if err == nil && renewed.HeartbeatAt.After(initial.HeartbeatAt) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || !renewed.HeartbeatAt.After(initial.HeartbeatAt) {
		t.Fatalf("lease 未跨真实 heartbeat interval 续租: initial=%#v renewed=%#v err=%v", initial, renewed, err)
	}
	if !renewed.ExpiresAt.After(initial.ExpiresAt) || !renewed.LiveAt(initial.ExpiresAt) ||
		renewed.TurnID != initial.TurnID || renewed.LeaseToken != initial.LeaseToken {
		t.Fatalf("heartbeat 未延长同一 owner 的 lease: initial=%#v renewed=%#v", initial, renewed)
	}

	session.close()
	if _, err := storage.GetAgentEditLease(
		t.Context(), database.Read(), draftID,
	); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("heartbeat 测试结束后 lease 未释放: %v", err)
	}
}

func TestRunTurnStopsAfterHeartbeatLeaseLossWithoutReacquire(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const (
		draftID   = "draft-run-turn-heartbeat-loss"
		messageID = "message-run-turn-heartbeat-loss"
	)
	agenttest.CreateAgentDraft(t, database, draftID)
	provider := &leaseLossBlockingProvider{entered: make(chan struct{})}
	service, err := NewService(t.Context(), database, provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "asset_surface", AssetKind: "video",
		SourceStartFrame: 0, SourceEndFrame: 60, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	seedCtx := rushestools.WithTimelineMutationOrigin(
		rushestools.WithDraftID(t.Context(), draftID), "manual",
	)
	if persisted, persistErr := seedTimelineVersion(
		service, seedCtx, draftID, document, "lease_loss_fixture", nil,
	); persistErr != nil || persisted.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("seed timeline=%#v err=%v", persisted, persistErr)
	}
	agenttest.InsertAgentMessage(
		t, database, draftID, messageID, "只修改时间线片段音量",
	)
	if !service.Queue().EnqueueUserMessage(draftID, messageID, "只修改时间线片段音量") {
		t.Fatal("enqueue failed")
	}

	select {
	case <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider 未进入持有 edit lease 的模型调用")
	}
	original, err := storage.GetLiveAgentEditLease(
		t.Context(), database.Read(), draftID, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	stolenAt := time.Now().UTC()
	const leaseTimeLayout = "2006-01-02T15:04:05.000000000Z"
	if _, err := database.Write().ExecContext(t.Context(), `
		UPDATE agent_edit_leases
		SET turn_id='turn-stolen',lease_token='lease-stolen',heartbeat_at=?,expires_at=?
		WHERE draft_id=? AND turn_id=? AND lease_token=?`,
		stolenAt.Format(leaseTimeLayout),
		stolenAt.Add(agentEditLeaseTTL).Format(leaseTimeLayout),
		draftID, original.TurnID, original.LeaseToken,
	); err != nil {
		t.Fatal(err)
	}

	drained := make(chan struct{})
	go func() {
		service.Queue().JoinDraft(draftID)
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(agentEditLeaseHeartbeatInterval + agentEditLeaseCleanupTimeout + 5*time.Second):
		t.Fatal("heartbeat CAS 丢失后 runTurn 未停止")
	}

	calls, bound := provider.snapshot()
	if calls != 1 || !containsName(bound, "timeline.update") {
		t.Fatalf("lease 丢失后发生额外 provider 调用或未进入编辑面: calls=%d bound=%v", calls, bound)
	}
	stolen, err := storage.GetAgentEditLease(t.Context(), database.Read(), draftID)
	if err != nil || stolen.TurnID != "turn-stolen" || stolen.LeaseToken != "lease-stolen" {
		t.Fatalf("旧 session 释放或重获了新 owner lease: lease=%#v err=%v", stolen, err)
	}
	run, err := storage.GetAgentTurnRunBySource(
		t.Context(), database.Read(), draftID, messageID, string(QueueUserMessage),
	)
	if err != nil || run.Status != "lease_lost" {
		t.Fatalf("turn run=%#v err=%v", run, err)
	}
	var versions, receipts, toolMessages int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT
			(SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?),
			(SELECT COUNT(*) FROM agent_tool_receipts WHERE draft_id=?),
			(SELECT COUNT(*) FROM messages WHERE draft_id=? AND kind='tool')`,
		draftID, draftID, draftID,
	).Scan(&versions, &receipts, &toolMessages); err != nil {
		t.Fatal(err)
	}
	if versions != 1 || receipts != 0 || toolMessages != 0 {
		t.Fatalf("lease 丢失后仍执行工具或 mutation: versions=%d receipts=%d tools=%d", versions, receipts, toolMessages)
	}
}

func TestDirectAgentTimelineMutationWithoutTurnLeaseIsRejected(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft-direct-agent-mutation-no-lease"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	if _, err := service.ExecuteTool(ctx, "timeline.insert", rushestools.TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "asset_surface",
		"source_start_frame": 0, "source_end_frame": 30,
	}); !errors.Is(err, storage.ErrAgentEditLeaseLost) {
		t.Fatalf("direct Agent mutation err=%v", err)
	}
	var versions, events, history int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT
			(SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?),
			(SELECT COUNT(*) FROM event_log WHERE draft_id=? AND event_type='TimelineVersionCreated'),
			(SELECT COUNT(*) FROM timeline_edit_batches WHERE draft_id=?)`,
		draftID, draftID, draftID,
	).Scan(&versions, &events, &history); err != nil || versions != 0 || events != 0 || history != 0 {
		t.Fatalf("rejected direct mutation leaked: versions=%d events=%d history=%d err=%v", versions, events, history, err)
	}
}

func TestDiscoveryProviderCallsNeverAcquireTimelineEditLease(t *testing.T) {
	for index, prompt := range []string{
		"理解素材",
		"查看素材",
		"搜索镜头",
	} {
		t.Run(prompt, func(t *testing.T) {
			database := agenttest.AgentTestDatabase(t)
			draftID := fmt.Sprintf("draft-discovery-no-lease-%d", index)
			agenttest.CreateAgentDraft(t, database, draftID)
			service, err := NewService(t.Context(), database, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(service.Close)
			seedSurfaceAsset(t, service, draftID)

			turnCtx, cancelTurn := context.WithCancelCause(t.Context())
			session := newTimelineEditLeaseSession(
				database, draftID, "turn-discovery-no-lease", cancelTurn,
			)
			t.Cleanup(func() {
				session.close()
				cancelTurn(nil)
			})
			turnCtx = rushestools.WithDraftID(turnCtx, draftID)
			turnCtx = withTimelineEditLeaseSession(turnCtx, session)
			spy := &editLeaseProviderSpy{
				database: database, draftID: draftID, expectedLive: []bool{false},
			}
			surface := &dynamicToolSurfaceModel{inner: spy, registry: service.tools}
			if _, err := surface.Generate(
				turnCtx, []*schema.Message{schema.UserMessage(prompt)},
			); err != nil {
				t.Fatal(err)
			}
			if spy.calls != 1 || len(spy.bound) != 1 {
				t.Fatalf("provider calls=%d bound=%v", spy.calls, spy.bound)
			}
			if containsName(spy.bound[0], "timeline.insert") {
				t.Fatalf("Discovery 泄露 timeline mutation: %v", spy.bound[0])
			}
			if session.activeTurnID() != "" {
				t.Fatalf("纯分析调用提前取得 edit lease: %q", session.activeTurnID())
			}
			if _, err := storage.GetLiveAgentEditLease(
				t.Context(), database.Read(), draftID, time.Now().UTC(),
			); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("纯分析调用后 live edit lease err=%v", err)
			}
		})
	}
}

func TestReadOnlyBeatAndTranscriptAnalysisNeverAcquireTimelineEditLease(t *testing.T) {
	for index, test := range []struct {
		prompt   string
		toolName string
		exact    bool
	}{
		{prompt: "分析 BGM 拍点", toolName: "audio.analyze_beats"},
		{
			prompt:   "分析当前卡点混剪的 BGM 素材 asset_surface，返回完整可用拍点证据；只做分析，不编辑时间线。",
			toolName: "audio.analyze_beats",
			exact:    true,
		},
		{prompt: "读取口播逐字稿", toolName: "speech.transcribe"},
	} {
		t.Run(test.prompt, func(t *testing.T) {
			database := agenttest.AgentTestDatabase(t)
			draftID := fmt.Sprintf("draft-read-analysis-no-lease-%d", index)
			agenttest.CreateAgentDraft(t, database, draftID)
			service, err := NewService(t.Context(), database, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(service.Close)
			seedSurfaceAsset(t, service, draftID)

			turnCtx, cancelTurn := context.WithCancelCause(t.Context())
			session := newTimelineEditLeaseSession(
				database, draftID, "turn-read-analysis-no-lease", cancelTurn,
			)
			t.Cleanup(func() {
				session.close()
				cancelTurn(nil)
			})
			turnCtx = rushestools.WithDraftID(turnCtx, draftID)
			turnCtx = withTimelineEditLeaseSession(turnCtx, session)
			spy := &editLeaseProviderSpy{
				database: database, draftID: draftID, expectedLive: []bool{false, false},
			}
			surface := &dynamicToolSurfaceModel{inner: spy, registry: service.tools}
			user := schema.UserMessage(test.prompt)
			if _, err := surface.Generate(turnCtx, []*schema.Message{user}); err != nil {
				t.Fatal(err)
			}
			if !containsName(spy.bound[0], test.toolName) {
				t.Fatalf("只读分析缺少 %s: %v", test.toolName, spy.bound[0])
			}
			if test.exact && (len(spy.bound[0]) != 1 || spy.bound[0][0] != test.toolName) {
				t.Fatalf("真实只读分析工具面=%v want=[%s]", spy.bound[0], test.toolName)
			}
			if containsTimelineMutationTool(spy.bound[0]) {
				t.Fatalf("只读分析泄露 mutation: %v", spy.bound[0])
			}
			if _, err := surface.Generate(turnCtx, []*schema.Message{
				user,
				schema.ToolMessage(
					`{"status":"succeeded"}`,
					"call-read-analysis",
					schema.WithToolName(test.toolName),
				),
			}); err != nil {
				t.Fatal(err)
			}
			if spy.calls != 2 || containsTimelineMutationTool(spy.bound[1]) {
				t.Fatalf("分析终态后的工具面=%v calls=%d", spy.bound[1], spy.calls)
			}
			if test.exact && (len(spy.bound[1]) != 1 || spy.bound[1][0] != test.toolName) {
				t.Fatalf("真实只读分析终态工具面=%v want=[%s]", spy.bound[1], test.toolName)
			}
			if session.activeTurnID() != "" {
				t.Fatalf("纯分析调用提前取得 edit lease: %q", session.activeTurnID())
			}
			if _, err := storage.GetLiveAgentEditLease(
				t.Context(), database.Read(), draftID, time.Now().UTC(),
			); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("纯分析调用后 live edit lease err=%v", err)
			}
		})
	}
}

func TestMixedBeatAnalysisAndTimelineEditBindsMutationUnderLease(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft-mixed-beat-edit-lease"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)

	turnCtx, cancelTurn := context.WithCancelCause(t.Context())
	session := newTimelineEditLeaseSession(database, draftID, "turn-mixed-beat-edit", cancelTurn)
	t.Cleanup(func() {
		session.close()
		cancelTurn(nil)
	})
	turnCtx = rushestools.WithDraftID(turnCtx, draftID)
	turnCtx = withTimelineEditLeaseSession(turnCtx, session)
	spy := &editLeaseProviderSpy{
		database: database, draftID: draftID, expectedLive: []bool{true},
	}
	surface := &dynamicToolSurfaceModel{inner: spy, registry: service.tools}
	if _, err := surface.Generate(turnCtx, []*schema.Message{
		schema.UserMessage("分析 BGM 拍点并把它加到时间线"),
	}); err != nil {
		t.Fatal(err)
	}
	if spy.calls != 1 || !containsName(spy.bound[0], "audio.analyze_beats") ||
		!containsName(spy.bound[0], "timeline.insert") {
		t.Fatalf("混合分析编辑工具面=%v calls=%d", spy.bound[0], spy.calls)
	}
	lease, err := storage.GetLiveAgentEditLease(
		t.Context(), database.Read(), draftID, time.Now().UTC(),
	)
	if err != nil || lease.TurnID != "turn-mixed-beat-edit" {
		t.Fatalf("混合编辑未在 mutation 工具披露前持有 lease: lease=%#v err=%v", lease, err)
	}
}

func TestBeatAnalysisThenCardEditBindsMutationUnderLease(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft-beat-analysis-then-card-edit"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)

	turnCtx, cancelTurn := context.WithCancelCause(t.Context())
	session := newTimelineEditLeaseSession(
		database, draftID, "turn-beat-analysis-then-card-edit", cancelTurn,
	)
	t.Cleanup(func() {
		session.close()
		cancelTurn(nil)
	})
	turnCtx = rushestools.WithDraftID(turnCtx, draftID)
	turnCtx = withTimelineEditLeaseSession(turnCtx, session)
	spy := &editLeaseProviderSpy{
		database: database, draftID: draftID, expectedLive: []bool{true},
	}
	surface := &dynamicToolSurfaceModel{inner: spy, registry: service.tools}
	if _, err := surface.Generate(turnCtx, []*schema.Message{
		schema.UserMessage("分析 BGM 后做卡点"),
	}); err != nil {
		t.Fatal(err)
	}
	if spy.calls != 1 || !containsName(spy.bound[0], "audio.analyze_beats") ||
		!containsName(spy.bound[0], "timeline.insert") {
		t.Fatalf("分析后编辑工具面=%v calls=%d", spy.bound[0], spy.calls)
	}
	lease, err := storage.GetLiveAgentEditLease(
		t.Context(), database.Read(), draftID, time.Now().UTC(),
	)
	if err != nil || lease.TurnID != "turn-beat-analysis-then-card-edit" {
		t.Fatalf("分析后编辑未在 provider 前持有 lease: lease=%#v err=%v", lease, err)
	}
}

func containsTimelineMutationTool(names []string) bool {
	for _, name := range names {
		if toolRequiresTimelineEditLease(name) {
			return true
		}
	}
	return false
}

func TestTimelineEditLeaseSessionDefensiveStateTransitions(t *testing.T) {
	if (*timelineEditLeaseSession)(nil).ensure(t.Context()) != nil ||
		(*timelineEditLeaseSession)(nil).activeTurnID() != "" {
		t.Fatal("nil lease session should be an inert harness boundary")
	}
	(*timelineEditLeaseSession)(nil).markLost(nil)
	(*timelineEditLeaseSession)(nil).close()

	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft-lease-defensive-state"
	agenttest.CreateAgentDraft(t, database, draftID)
	turnCtx, cancelTurn := context.WithCancelCause(t.Context())
	session := newTimelineEditLeaseSession(database, draftID, "turn-defensive-state", cancelTurn)
	if err := session.ensure(turnCtx); err != nil {
		t.Fatal(err)
	}
	if err := session.ensure(turnCtx); err != nil {
		t.Fatalf("idempotent ensure: %v", err)
	}
	session.markLost(nil)
	if !errors.Is(context.Cause(turnCtx), storage.ErrAgentEditLeaseLost) ||
		!errors.Is(session.ensure(turnCtx), storage.ErrAgentEditLeaseLost) ||
		session.activeTurnID() != "" {
		t.Fatalf("lost session cause=%v active=%q", context.Cause(turnCtx), session.activeTurnID())
	}
	session.markLost(errors.New("duplicate loss must be ignored"))
	session.close()
	session.close()
	if !errors.Is(session.ensure(t.Context()), storage.ErrAgentEditLeaseLost) {
		t.Fatal("closed session was reacquired")
	}

	neverAcquired := newTimelineEditLeaseSession(database, draftID, "turn-never-acquired", func(error) {})
	neverAcquired.close()
	if !errors.Is(neverAcquired.ensure(t.Context()), storage.ErrAgentEditLeaseLost) {
		t.Fatal("closed unacquired session was reacquired")
	}

	heartbeatCtx, stopHeartbeat := context.WithCancel(t.Context())
	stopHeartbeat()
	heartbeatDone := make(chan struct{})
	go func() {
		neverAcquired.heartbeat(heartbeatCtx)
		close(heartbeatDone)
	}()
	select {
	case <-heartbeatDone:
	case <-time.After(time.Second):
		t.Fatal("cancelled heartbeat did not stop")
	}
}

func TestTimelineEditLeaseHelpersAndCancelledStorageMutation(t *testing.T) {
	if toolRequiresTimelineEditLease("timeline.inspect") ||
		!toolRequiresTimelineEditLease("preview.generate") ||
		specsRequireTimelineEditLease([]rushestools.Spec{{Name: "timeline.inspect"}}) ||
		!specsRequireTimelineEditLease([]rushestools.Spec{
			{Name: "timeline.inspect"}, {Name: "timeline.split"},
		}) {
		t.Fatal("lease helper classification mismatch")
	}

	database := agenttest.AgentTestDatabase(t)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := expireStaleAgentEditLeases(cancelled, database); !errors.Is(err, context.Canceled) {
		t.Fatalf("expire stale cancelled err=%v", err)
	}
}

func TestCompositeDiscoveryAcquiresLeaseOnlyWhenMutationSurfaceIsBound(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft-composite-lazy-edit-lease"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	seedSurfaceAsset(t, service, draftID)

	turnCtx, cancelTurn := context.WithCancelCause(t.Context())
	session := newTimelineEditLeaseSession(
		database, draftID, "turn-composite-lazy-lease", cancelTurn,
	)
	t.Cleanup(func() {
		session.close()
		cancelTurn(nil)
	})
	turnCtx = rushestools.WithDraftID(turnCtx, draftID)
	turnCtx = withTimelineEditLeaseSession(turnCtx, session)
	spy := &editLeaseProviderSpy{
		database: database, draftID: draftID, expectedLive: []bool{false, true},
	}
	surface := &dynamicToolSurfaceModel{inner: spy, registry: service.tools}
	user := schema.UserMessage("先搜索镜头，再把选中的镜头插入时间线")
	if _, err := surface.Generate(turnCtx, []*schema.Message{user}); err != nil {
		t.Fatal(err)
	}
	if containsName(spy.bound[0], "timeline.insert") ||
		!containsName(spy.bound[0], "shot.search") {
		t.Fatalf("证据阶段工具面=%v", spy.bound[0])
	}
	if _, err := surface.Generate(turnCtx, []*schema.Message{
		user,
		schema.ToolMessage(
			`{"shots":[{"shot_id":"shot-a"}]}`,
			"call-shot-search",
			schema.WithToolName("shot.search"),
		),
	}); err != nil {
		t.Fatal(err)
	}
	if spy.calls != 2 || len(spy.bound) != 2 {
		t.Fatalf("provider calls=%d bound=%v", spy.calls, spy.bound)
	}
	if !containsName(spy.bound[1], "timeline.insert") {
		t.Fatalf("证据完成后未绑定 mutation: %v", spy.bound[1])
	}
	lease, err := storage.GetLiveAgentEditLease(
		t.Context(), database.Read(), draftID, time.Now().UTC(),
	)
	if err != nil || lease.TurnID != "turn-composite-lazy-lease" {
		t.Fatalf("mutation provider 未持有预期 edit lease: lease=%#v err=%v", lease, err)
	}
}

func testNow() time.Time { return time.Now().UTC() }
