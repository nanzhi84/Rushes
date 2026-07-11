package worker

import (
	"context"
	"fmt"
	"sort"
)

type ProgressReporter func(context.Context, Job, float64) error

type Handler func(context.Context, Job, ProgressReporter) (map[string]any, error)

type Registry struct {
	handlers map[string]Handler
}

func NewRegistry() *Registry {
	return &Registry{handlers: map[string]Handler{}}
}

func (registry *Registry) Register(kind string, handler Handler) error {
	if kind == "" || handler == nil {
		return fmt.Errorf("job handler 注册参数无效")
	}
	if _, exists := registry.handlers[kind]; exists {
		return fmt.Errorf("job handler 已注册: %s", kind)
	}
	registry.handlers[kind] = handler
	return nil
}

func (registry *Registry) Require(kind string) (Handler, error) {
	handler, ok := registry.handlers[kind]
	if !ok {
		return nil, fmt.Errorf("job handler 未注册: %s", kind)
	}
	return handler, nil
}

func (registry *Registry) Kinds() []string {
	kinds := make([]string, 0, len(registry.handlers))
	for kind := range registry.handlers {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}
