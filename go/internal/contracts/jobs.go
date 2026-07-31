package contracts

import "sort"

type JobExecutionClass string

const (
	JobExecutionGeneral JobExecutionClass = "general"
	JobExecutionRender  JobExecutionClass = "render"
)

type JobKindSpec struct {
	Kind           string
	AgentWaited    bool
	ProgressLabel  string
	ExecutionClass JobExecutionClass
}

var jobKindRegistry = map[string]JobKindSpec{
	"ingest": {
		Kind: "ingest", ExecutionClass: JobExecutionGeneral,
	},
	"understand": {
		Kind: "understand", AgentWaited: false, ProgressLabel: "理解素材",
		ExecutionClass: JobExecutionGeneral,
	},
	"render_preview": {
		Kind: "render_preview", AgentWaited: false, ProgressLabel: "渲染预览",
		ExecutionClass: JobExecutionRender,
	},
	"render_final": {
		Kind: "render_final", AgentWaited: false, ProgressLabel: "渲染成片",
		ExecutionClass: JobExecutionRender,
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
