package contracts

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

type jobKindGolden struct {
	All              []string            `json:"all"`
	AgentWaited      []string            `json:"agent_waited"`
	ExecutionClasses map[string][]string `json:"execution_classes"`
	ProgressLabels   map[string]string   `json:"progress_labels"`
}

func TestJobKindRegistryMatchesSharedGolden(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/job_kinds.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden jobKindGolden
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	actual := jobKindGolden{
		ExecutionClasses: map[string][]string{
			string(JobExecutionGeneral): JobKindsByExecutionClass(JobExecutionGeneral),
			string(JobExecutionRender):  JobKindsByExecutionClass(JobExecutionRender),
		},
		ProgressLabels: map[string]string{},
	}
	for _, spec := range AllJobKindSpecs() {
		actual.All = append(actual.All, spec.Kind)
		if spec.AgentWaited {
			actual.AgentWaited = append(actual.AgentWaited, spec.Kind)
			actual.ProgressLabels[spec.Kind] = spec.ProgressLabel
			if spec.ContinuationHint == "" {
				t.Errorf("agent-waited job %s 缺少 continuation hint", spec.Kind)
			}
		}
		if spec.ExecutionClass != JobExecutionGeneral && spec.ExecutionClass != JobExecutionRender {
			t.Errorf("job %s execution class=%q", spec.Kind, spec.ExecutionClass)
		}
	}
	if !reflect.DeepEqual(actual, golden) {
		t.Fatalf("job kind golden 漂移\nactual=%#v\ngolden=%#v", actual, golden)
	}
}
