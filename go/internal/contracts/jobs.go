package contracts

import "sort"

type JobExecutionClass string

const (
	JobExecutionGeneral JobExecutionClass = "general"
	JobExecutionRender  JobExecutionClass = "render"
)

const DefaultJobContinuationHint = "读取真实产物；若产物是预览，使用返回的 preview_id 分别调用 preview.check 的 decode、black、freeze、silence、loudness 原子检查，独立检查可并行；需要构图、B-roll 语义或字幕遮挡证据时再单独调用 visual。只根据明确失败项决定是否修订。"

type JobKindSpec struct {
	Kind             string
	AgentWaited      bool
	ContinuationHint string
	ProgressLabel    string
	ExecutionClass   JobExecutionClass
}

var jobKindRegistry = map[string]JobKindSpec{
	"ingest": {
		Kind: "ingest", ExecutionClass: JobExecutionGeneral,
	},
	"understand": {
		Kind: "understand", AgentWaited: true, ProgressLabel: "理解素材",
		ExecutionClass:   JobExecutionGeneral,
		ContinuationHint: "优先读取紧邻本消息前的【本次后台素材理解结果（SQLite 持久化事实）】定向证据；assets.material_catalog 只作常驻目录补充且可能截断。依据其中的 overall 与 semantic_tags 继续原任务，需要逐镜头细节时再调用 shot.search。不要重复调用 media.detect_shots，也不要只报告后台完成。",
	},
	"render_preview": {
		Kind: "render_preview", AgentWaited: true, ProgressLabel: "渲染预览",
		ExecutionClass: JobExecutionRender, ContinuationHint: DefaultJobContinuationHint,
	},
	"render_final": {
		Kind: "render_final", AgentWaited: true, ProgressLabel: "渲染成片",
		ExecutionClass: JobExecutionRender, ContinuationHint: DefaultJobContinuationHint,
	},
}

func LookupJobKind(kind string) (JobKindSpec, bool) {
	spec, exists := jobKindRegistry[kind]
	return spec, exists
}

func AllJobKindSpecs() []JobKindSpec {
	result := make([]JobKindSpec, 0, len(jobKindRegistry))
	for _, spec := range jobKindRegistry {
		result = append(result, spec)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Kind < result[j].Kind })
	return result
}

func JobKindsByExecutionClass(class JobExecutionClass) []string {
	result := []string{}
	for _, spec := range AllJobKindSpecs() {
		if spec.ExecutionClass == class {
			result = append(result, spec.Kind)
		}
	}
	return result
}
