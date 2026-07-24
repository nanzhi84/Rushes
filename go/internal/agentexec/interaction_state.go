package agentexec

import (
	"context"
	"sort"
	"sync"

	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type turnInteractionContextKey struct{}

type turnInteractionState struct {
	// executionMu 让普通副作用工具继续与本回合所有工具互斥；纯读与资源隔离 detector
	// 共享它后，再由 domain/resource 锁保护 detector 持久化索引。
	executionMu      sync.RWMutex
	indexedResources *IndexedResourceCoordinator
	mu               sync.Mutex
	createdDecisions map[string]struct{}
	blockingDecision string
}

// IndexedResourceCoordinator 在 Service 生命周期内协调所有 turn/draft 对全局素材索引的
// 访问。transcript/material index 按 asset 存储而非按 draft 存储，因此锁不能只活在单回合。
type IndexedResourceCoordinator struct {
	mu            sync.Mutex
	domainLocks   map[string]*indexedDomainLock
	resourceLocks map[string]*sync.RWMutex
}

type IndexedResourceAccess struct {
	Domain        string
	Resources     []string
	WriteResource bool
	AllResources  bool
}

// indexedDomainLock 只协调“全域读取”和“单资源写入”：全域读取之间共享，
// 单资源写入之间仍可并行，但两者互斥。限定资源的读写冲突继续交给 resourceLocks。
// waitingWriters 阻止持续到来的全域读取饿死已经等待的 detector。
type indexedDomainLock struct {
	mu             sync.Mutex
	cond           *sync.Cond
	allReaders     int
	activeWriters  int
	waitingWriters int
}

func newIndexedDomainLock() *indexedDomainLock {
	lock := &indexedDomainLock{}
	lock.cond = sync.NewCond(&lock.mu)
	return lock
}

func (lock *indexedDomainLock) beginAllRead() {
	lock.mu.Lock()
	for lock.activeWriters > 0 || lock.waitingWriters > 0 {
		lock.cond.Wait()
	}
	lock.allReaders++
	lock.mu.Unlock()
}

func (lock *indexedDomainLock) endAllRead() {
	lock.mu.Lock()
	lock.allReaders--
	if lock.allReaders == 0 {
		lock.cond.Broadcast()
	}
	lock.mu.Unlock()
}

func (lock *indexedDomainLock) beginResourceWrite() {
	lock.mu.Lock()
	lock.waitingWriters++
	for lock.allReaders > 0 {
		lock.cond.Wait()
	}
	lock.waitingWriters--
	lock.activeWriters++
	lock.mu.Unlock()
}

func (lock *indexedDomainLock) endResourceWrite() {
	lock.mu.Lock()
	lock.activeWriters--
	if lock.activeWriters == 0 {
		lock.cond.Broadcast()
	}
	lock.mu.Unlock()
}

func NewIndexedResourceCoordinator() *IndexedResourceCoordinator {
	return &IndexedResourceCoordinator{
		domainLocks:   map[string]*indexedDomainLock{},
		resourceLocks: map[string]*sync.RWMutex{},
	}
}

func NewTurnInteractionState(
	sharedCoordinators ...*IndexedResourceCoordinator,
) *turnInteractionState {
	coordinator := (*IndexedResourceCoordinator)(nil)
	if len(sharedCoordinators) > 0 {
		coordinator = sharedCoordinators[0]
	}
	if coordinator == nil {
		coordinator = NewIndexedResourceCoordinator()
	}
	return &turnInteractionState{
		createdDecisions: map[string]struct{}{},
		indexedResources: coordinator,
	}
}

func WithTurnInteractionState(ctx context.Context, state *turnInteractionState) context.Context {
	return context.WithValue(ctx, turnInteractionContextKey{}, state)
}

func InteractionStateFromContext(ctx context.Context) *turnInteractionState {
	state, _ := ctx.Value(turnInteractionContextKey{}).(*turnInteractionState)
	return state
}

func MarkDecisionCreatedThisTurn(ctx context.Context, decisionID string, blocking bool) {
	state := InteractionStateFromContext(ctx)
	if state == nil || decisionID == "" {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.createdDecisions[decisionID] = struct{}{}
	if blocking {
		state.blockingDecision = decisionID
	}
}

func decisionCreatedThisTurn(ctx context.Context, decisionID string) bool {
	state := InteractionStateFromContext(ctx)
	if state == nil || decisionID == "" {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	_, exists := state.createdDecisions[decisionID]
	return exists
}

func nullableToolCallID(ctx context.Context) any {
	if toolCallID := rushestools.ToolCallID(ctx); toolCallID != "" {
		return toolCallID
	}
	return nil
}

func NormalizeDecisionType(value string) string {
	switch value {
	case "critical", "approve_content_plan", "approve_speech_cut", "approve_rough_cut":
		return value
	default:
		return "generic"
	}
}

// BeginToolCall 取得本回合工具执行互斥,并返回释放函数与当前阻塞决策 ID。readOnly 为真时
// 取共享 RLock(只读工具可并发),否则取独占 Lock(副作用工具串行,#103 G3b)。屏障检查始终
// 进行:在 state.mu 下读 blockingDecision 供引擎短路。引擎侧装饰器 beginTurnToolCall 读取
// ctx 后调用它,把决策屏障语义留在引擎、状态内部字段留在领域包。
func (state *turnInteractionState) BeginToolCall(readOnly bool) (func(), string) {
	var release func()
	if readOnly {
		state.executionMu.RLock()
		release = state.executionMu.RUnlock
	} else {
		state.executionMu.Lock()
		release = state.executionMu.Unlock
	}
	state.mu.Lock()
	decisionID := state.blockingDecision
	state.mu.Unlock()
	return release, decisionID
}

// BeginIndexedToolCall 取得 detector 索引的分层锁。detector 对单个资源写入时持有领域
// 写屏障和资源独占锁；限定资源的搜索只持有资源共享锁；无法限定资源的全域搜索持有
// 可共享的全域读许可。三者都先持有 executionMu 的共享锁，因此不同资源 detector 与
// 多个纯读调用可以并发，同资源 detector/搜索及全域读/detector 会互斥，普通
// edit/control 仍通过 BeginToolCall(false) 全局互斥。
func (state *turnInteractionState) BeginIndexedToolCall(
	domain string, resources []string,
	writeResource, allResources bool,
) (func(), string) {
	return state.BeginIndexedToolCalls([]IndexedResourceAccess{{
		Domain: domain, Resources: resources,
		WriteResource: writeResource, AllResources: allResources,
	}})
}

// BeginIndexedToolCalls 在一次 executionMu 共享锁下按领域名稳定加锁，使一个只读调用
// 可以原子声明多个索引依赖，同时避免多个 RLock 与等待中的全局写锁形成递归读死锁。
func (state *turnInteractionState) BeginIndexedToolCalls(
	accesses []IndexedResourceAccess,
) (func(), string) {
	state.executionMu.RLock()
	releaseIndexed := state.indexedResources.Begin(accesses)
	state.mu.Lock()
	decisionID := state.blockingDecision
	state.mu.Unlock()
	return func() {
		releaseIndexed()
		state.executionMu.RUnlock()
	}, decisionID
}

// Begin 按稳定的领域/资源顺序取得共享索引锁。不同 turn、不同 draft 只要通过同一
// Service，就会在这里按全局 asset_id 判冲突；不同资源的 detector 仍可并行。
func (coordinator *IndexedResourceCoordinator) Begin(
	accesses []IndexedResourceAccess,
) func() {
	accesses = append([]IndexedResourceAccess(nil), accesses...)
	sort.Slice(accesses, func(left, right int) bool {
		return accesses[left].Domain < accesses[right].Domain
	})
	type heldAccess struct {
		access        IndexedResourceAccess
		domainLock    *indexedDomainLock
		resourceLocks []*sync.RWMutex
	}
	held := make([]heldAccess, 0, len(accesses))
	for _, access := range accesses {
		domainLock, resourceLocks := coordinator.indexLocks(access.Domain, access.Resources)
		switch {
		case access.AllResources:
			domainLock.beginAllRead()
		case access.WriteResource:
			domainLock.beginResourceWrite()
			resourceLocks[0].Lock()
		default:
			for _, resourceLock := range resourceLocks {
				resourceLock.RLock()
			}
		}
		held = append(held, heldAccess{
			access: access, domainLock: domainLock, resourceLocks: resourceLocks,
		})
	}
	return func() {
		for heldIndex := len(held) - 1; heldIndex >= 0; heldIndex-- {
			item := held[heldIndex]
			switch {
			case item.access.AllResources:
				item.domainLock.endAllRead()
			case item.access.WriteResource:
				item.resourceLocks[0].Unlock()
				item.domainLock.endResourceWrite()
			default:
				for index := len(item.resourceLocks) - 1; index >= 0; index-- {
					item.resourceLocks[index].RUnlock()
				}
			}
		}
	}
}

func (coordinator *IndexedResourceCoordinator) indexLocks(
	domain string, resources []string,
) (*indexedDomainLock, []*sync.RWMutex) {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	domainLock := coordinator.domainLocks[domain]
	if domainLock == nil {
		domainLock = newIndexedDomainLock()
		coordinator.domainLocks[domain] = domainLock
	}
	resources = append([]string(nil), resources...)
	sort.Strings(resources)
	resourceLocks := make([]*sync.RWMutex, 0, len(resources))
	previous := ""
	for _, resource := range resources {
		if resource == previous {
			continue
		}
		previous = resource
		key := domain + "\x00" + resource
		resourceLock := coordinator.resourceLocks[key]
		if resourceLock == nil {
			resourceLock = &sync.RWMutex{}
			coordinator.resourceLocks[key] = resourceLock
		}
		resourceLocks = append(resourceLocks, resourceLock)
	}
	return domainLock, resourceLocks
}
