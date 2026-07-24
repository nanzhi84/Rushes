package agent

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// toolRouter 按 Registry Spec 逐消息在并行/串行两个 ToolsNode 间路由。
// 纯读调用并行；会写证据的 detect 仅在每个调用都能解析出不同的资源键，且不与同消息
// 中读取对应索引的调用重叠时并行。edit/control、资源冲突、空调用、无资源键或未知工具
// 都串行保序。
//
// 两个 ToolsNode 共享同一 Tools 与中间件,仅 ExecuteSequentially 相反。把本路由挂成图节点
// （AddLambdaNode）的 react 图复刻在 impl-s1a 的 react 循环 PR 落地后基于其结构接入。
type toolRouter struct {
	parallel *compose.ToolsNode
	serial   *compose.ToolsNode
	specOf   func(name string) (rushestools.Spec, bool)
}

// newToolRouter 用同一份 ToolsNodeConfig 构造并行与串行两个执行节点,读写分类事实源是
// Registry Spec（经 specOf 注入），不建第二清单。
func newToolRouter(
	ctx context.Context,
	config compose.ToolsNodeConfig,
	specOf func(string) (rushestools.Spec, bool),
) (*toolRouter, error) {
	parallelConfig := config
	parallelConfig.ExecuteSequentially = false
	serialConfig := config
	serialConfig.ExecuteSequentially = true
	parallel, err := compose.NewToolNode(ctx, &parallelConfig)
	if err != nil {
		return nil, err
	}
	serial, err := compose.NewToolNode(ctx, &serialConfig)
	if err != nil {
		return nil, err
	}
	return &toolRouter{
		parallel: parallel,
		serial:   serial,
		specOf:   specOf,
	}, nil
}

// canRunParallel 报告整条消息是否只包含纯读或资源隔离的 detector 调用。
func (router *toolRouter) canRunParallel(message *schema.Message) bool {
	if message == nil || len(message.ToolCalls) == 0 {
		return false
	}
	detectResources := make(map[string]struct{})
	readResources := make(map[string]struct{})
	for _, call := range message.ToolCalls {
		spec, exists := router.specOf(call.Function.Name)
		if !exists {
			return false
		}
		footprint, valid := indexedResourceFootprint(call.Function.Name, call.Function.Arguments)
		if spec.Effect == rushestools.EffectReadOnly {
			for _, access := range footprint {
				for _, resource := range access.resourceKeys() {
					readResources[resource] = struct{}{}
				}
			}
			continue
		}
		if spec.Family != rushestools.FamilyDetect ||
			spec.Effect != rushestools.EffectReversible {
			return false
		}
		if !valid || len(footprint) != 1 || !footprint[0].writeResource ||
			len(footprint[0].resources) != 1 {
			return false
		}
		resource := footprint[0].domain + "\x00" + footprint[0].resources[0]
		if _, duplicate := detectResources[resource]; duplicate {
			return false
		}
		detectResources[resource] = struct{}{}
	}
	for resource := range detectResources {
		if _, conflict := readResources[resource]; conflict {
			return false
		}
		domain, _, _ := strings.Cut(resource, "\x00")
		if _, conflict := readResources[domain+"\x00*"]; conflict {
			return false
		}
	}
	return true
}

type indexedResourceAccess struct {
	domain        string
	resources     []string
	writeResource bool
	allResources  bool
}

func (access indexedResourceAccess) resourceKeys() []string {
	if access.allResources {
		return []string{access.domain + "\x00*"}
	}
	keys := make([]string, 0, len(access.resources))
	for _, resource := range access.resources {
		keys = append(keys, access.domain+"\x00"+resource)
	}
	return keys
}

// indexedResourceFootprint 是路由判冲突和 Service 执行锁的共同事实源。
// 一个调用可以依赖多个持久化索引；无法静态解析资源范围时使用领域通配符。
func indexedResourceFootprint(name, rawArguments string) ([]indexedResourceAccess, bool) {
	var arguments map[string]any
	if json.Unmarshal([]byte(rawArguments), &arguments) != nil {
		return indexedResourceWildcard(name), false
	}
	switch name {
	case "media.detect_shots":
		if assetID := stringArgument(arguments, "asset_id"); assetID != "" {
			return []indexedResourceAccess{{
				domain: "shots", resources: []string{assetID}, writeResource: true,
			}}, true
		}
		return nil, false
	case "speech.transcribe":
		if assetID := stringArgument(arguments, "asset_id"); assetID != "" {
			return []indexedResourceAccess{{
				domain: "speech", resources: []string{assetID}, writeResource: true,
			}}, true
		}
		return nil, false
	case "speech.search":
		if assetID := stringArgument(arguments, "asset_id"); assetID != "" {
			return []indexedResourceAccess{{domain: "speech", resources: []string{assetID}}}, true
		}
		return []indexedResourceAccess{{domain: "speech", allResources: true}}, true
	case "shot.search":
		resources, scoped := assetIDsArgument(arguments, "asset_ids")
		return []indexedResourceAccess{
			{domain: "shots", resources: resources, allResources: !scoped},
			{domain: "speech", resources: resources, allResources: !scoped},
		}, true
	case "timeline.check":
		return []indexedResourceAccess{{domain: "speech", allResources: true}}, true
	case "preview.check":
		if stringArgument(arguments, "check") == "visual" {
			return []indexedResourceAccess{{domain: "speech", allResources: true}}, true
		}
		return nil, true
	default:
		return nil, true
	}
}

func indexedResourceWildcard(name string) []indexedResourceAccess {
	switch name {
	case "speech.search":
		return []indexedResourceAccess{{domain: "speech", allResources: true}}
	case "shot.search":
		return []indexedResourceAccess{
			{domain: "shots", allResources: true},
			{domain: "speech", allResources: true},
		}
	case "timeline.check", "preview.check":
		return []indexedResourceAccess{{domain: "speech", allResources: true}}
	default:
		return nil
	}
}

func assetIDsArgument(arguments map[string]any, field string) ([]string, bool) {
	values, ok := arguments[field].([]any)
	if !ok || len(values) == 0 {
		return nil, false
	}
	resources := make([]string, 0, len(values))
	for _, value := range values {
		assetID, ok := value.(string)
		assetID = strings.TrimSpace(assetID)
		if !ok || assetID == "" {
			return nil, false
		}
		resources = append(resources, assetID)
	}
	sort.Strings(resources)
	return resources, true
}

func stringArgument(arguments map[string]any, field string) string {
	value, _ := arguments[field].(string)
	return strings.TrimSpace(value)
}

// node 选择本条消息的执行节点：纯读/资源隔离 detector 走并行，其余走串行。
func (router *toolRouter) node(message *schema.Message) *compose.ToolsNode {
	if router.canRunParallel(message) {
		return router.parallel
	}
	return router.serial
}

// Invoke 把整条消息委派给所选节点。含写工具的消息走串行节点,由 eino 久经考验的串行路径保证
// 保序;全只读消息走并行节点,聚合仍按原下标保序。
func (router *toolRouter) Invoke(
	ctx context.Context, input *schema.Message, opts ...compose.ToolsNodeOption,
) ([]*schema.Message, error) {
	return router.node(input).Invoke(ctx, input, opts...)
}
