package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// toolDisclosureSession is an in-memory view of disclosure facts already present in
// the effective transcript. It is neither persisted nor authoritative: bind always
// replaces it with a fresh derivation from tool.load results before a provider call.
type toolDisclosureSession struct {
	mu                    sync.RWMutex
	loaded                map[string]struct{}
	latestLoadCompletedAt time.Time
	firstActionPending    bool
}

type toolDisclosureContextKey struct{}

func withToolDisclosureSession(ctx context.Context) context.Context {
	return context.WithValue(ctx, toolDisclosureContextKey{}, &toolDisclosureSession{
		loaded: map[string]struct{}{},
	})
}

func toolDisclosureSessionFromContext(ctx context.Context) *toolDisclosureSession {
	session, _ := ctx.Value(toolDisclosureContextKey{}).(*toolDisclosureSession)
	return session
}

func (session *toolDisclosureSession) replace(names []string) {
	if session == nil {
		return
	}
	loaded := make(map[string]struct{}, len(names))
	for _, name := range names {
		loaded[name] = struct{}{}
	}
	session.mu.Lock()
	session.loaded = loaded
	session.mu.Unlock()
}

func (session *toolDisclosureSession) snapshot() map[string]struct{} {
	result := map[string]struct{}{}
	if session == nil {
		return result
	}
	session.mu.RLock()
	defer session.mu.RUnlock()
	for name := range session.loaded {
		result[name] = struct{}{}
	}
	return result
}

func (session *toolDisclosureSession) add(names ...string) {
	if session == nil {
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.loaded == nil {
		session.loaded = map[string]struct{}{}
	}
	for _, name := range names {
		session.loaded[name] = struct{}{}
	}
}

func (session *toolDisclosureSession) recordLoadCompleted(now time.Time) {
	if session == nil {
		return
	}
	session.mu.Lock()
	session.latestLoadCompletedAt = now
	session.firstActionPending = true
	session.mu.Unlock()
}

func (session *toolDisclosureSession) takeFirstActionRoundtrip(now time.Time) (time.Duration, bool) {
	if session == nil {
		return 0, false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.firstActionPending || session.latestLoadCompletedAt.IsZero() {
		return 0, false
	}
	session.firstActionPending = false
	return now.Sub(session.latestLoadCompletedAt), true
}

// deterministicToolSchemaModel binds tool.load plus only those business schemas whose
// successful disclosure is present in the effective transcript. It never inspects user
// text, WorldState, preconditions, or workflow phases.
type deterministicToolSchemaModel struct {
	inner    model.ToolCallingChatModel
	registry *rushestools.Registry
}

func (surface *deterministicToolSchemaModel) WithTools(
	_ []*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	return surface, nil
}

func (surface *deterministicToolSchemaModel) Generate(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.Message, error) {
	bound, prepared, err := surface.bind(ctx, messages)
	if err != nil {
		return nil, err
	}
	return bound.Generate(ctx, prepared, options...)
}

func (surface *deterministicToolSchemaModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	bound, prepared, err := surface.bind(ctx, messages)
	if err != nil {
		return nil, err
	}
	return bound.Stream(ctx, prepared, options...)
}

func (surface *deterministicToolSchemaModel) bind(
	ctx context.Context,
	messages []*schema.Message,
) (model.ToolCallingChatModel, []*schema.Message, error) {
	if surface == nil || surface.inner == nil || surface.registry == nil {
		return nil, nil, errors.New("确定性工具 schema 加载缺少模型或 Registry")
	}
	loaded := loadedModelActionNames(messages, surface.registry)
	if session := toolDisclosureSessionFromContext(ctx); session != nil {
		session.replace(loaded)
	}
	implementations := make([]tool.BaseTool, 0, len(loaded)+1)
	loadSpec, exists := surface.registry.Spec("tool.load")
	if !exists || loadSpec.Exposure != rushestools.ExposureMeta {
		return nil, nil, errors.New("registry 缺少 tool.load")
	}
	implementations = append(implementations, loadSpec.Implementation)
	for _, name := range loaded {
		spec, ok := surface.registry.Spec(name)
		if !ok || spec.Exposure != rushestools.ExposureLLM {
			continue
		}
		implementations = append(implementations, spec.Implementation)
	}
	infos := make([]*schema.ToolInfo, 0, len(implementations))
	for _, implementation := range implementations {
		info, err := implementation.Info(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("读取确定性工具 schema: %w", err)
		}
		infos = append(infos, info)
	}
	bound, err := surface.inner.WithTools(infos)
	if err != nil {
		return nil, nil, err
	}
	recordBoundModelActionSchemas(ctx, implementations)
	prepared, err := refreshCurrentTimelineView(ctx, messages)
	if err != nil {
		return nil, nil, err
	}
	prepared, err = injectModelActionCatalog(prepared, surface.registry)
	if err != nil {
		return nil, nil, err
	}
	return bound, prepared, nil
}

func loadedModelActionNames(messages []*schema.Message, registry *rushestools.Registry) []string {
	valid := map[string]struct{}{}
	for _, name := range registry.ModelActionNames() {
		valid[name] = struct{}{}
	}
	loaded := map[string]struct{}{}
	for _, message := range messages {
		if message == nil || message.Role != schema.Tool || message.ToolName != "tool.load" {
			continue
		}
		var result rushestools.ToolLoadResult
		if json.Unmarshal([]byte(message.Content), &result) != nil ||
			result.Status != string(rushestools.StatusSucceeded) {
			continue
		}
		for _, name := range append(result.LoadedNames, result.AlreadyLoaded...) {
			if _, ok := valid[name]; ok {
				loaded[name] = struct{}{}
			}
		}
	}
	names := make([]string, 0, len(loaded))
	for name := range loaded {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func executeToolLoad(
	ctx context.Context,
	registry *rushestools.Registry,
	input rushestools.ToolLoadInput,
) (rushestools.ToolLoadResult, error) {
	startedAt := time.Now()
	defer func() { metricToolLoadDurationMS.Observe(time.Since(startedAt).Milliseconds()) }()
	if err := rushestools.ValidateToolLoadInput(input); err != nil {
		return rushestools.ToolLoadResult{}, err
	}
	valid := map[string]struct{}{}
	for _, name := range registry.ModelActionNames() {
		valid[name] = struct{}{}
	}
	current := toolDisclosureSessionFromContext(ctx).snapshot()
	seen := map[string]struct{}{}
	result := rushestools.ToolLoadResult{
		Status: string(rushestools.StatusSucceeded), LoadedNames: []string{},
		AlreadyLoaded: []string{}, NotLoadable: []string{},
	}
	for _, rawName := range input.ToolNames {
		name := strings.TrimSpace(rawName)
		seen[name] = struct{}{}
		if _, ok := valid[name]; !ok {
			result.NotLoadable = append(result.NotLoadable, name)
			continue
		}
		if _, already := current[name]; already {
			result.AlreadyLoaded = append(result.AlreadyLoaded, name)
			continue
		}
		result.LoadedNames = append(result.LoadedNames, name)
		current[name] = struct{}{}
	}
	if session := toolDisclosureSessionFromContext(ctx); session != nil {
		session.add(result.LoadedNames...)
		if len(result.LoadedNames) > 0 {
			session.recordLoadCompleted(time.Now())
		}
	}
	metricToolLoadTotal.Inc()
	metricToolLoadedCount.Observe(int64(len(current)))
	return result, nil
}

// loadedModelActionSpecs resolves transcript disclosure facts back to Registry specs.
// It does no intent, phase, precondition, or WorldState selection.
func loadedModelActionSpecs(
	_ context.Context,
	registry *rushestools.Registry,
	messages []*schema.Message,
) ([]rushestools.Spec, error) {
	specs := make([]rushestools.Spec, 0)
	for _, name := range loadedModelActionNames(messages, registry) {
		if spec, ok := registry.Spec(name); ok {
			specs = append(specs, spec)
		}
	}
	return specs, nil
}

func implementationsForSpecs(specs []rushestools.Spec) []tool.BaseTool {
	result := make([]tool.BaseTool, 0, len(specs))
	for _, spec := range specs {
		result = append(result, spec.Implementation)
	}
	return result
}
