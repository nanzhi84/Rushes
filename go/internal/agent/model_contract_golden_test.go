package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type modelContractGolden struct {
	Tools   []modelContractTool  `json:"tools"`
	Prompts modelContractPrompts `json:"prompts"`
}

type modelContractTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type modelContractPrompts struct {
	Core        string `json:"core"`
	Audio       string `json:"audio_track_playbook"`
	Beat        string `json:"beat_editing_playbook"`
	Timeline    string `json:"timeline_editing_playbook"`
	TalkingHead string `json:"talking_head_playbook"`
}

func TestModelContractGolden(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	contract := modelContractGolden{
		Prompts: modelContractPrompts{
			Core: coreSystemPrompt, Audio: agentexec.AudioTrackPlaybook, Beat: agentexec.BeatEditingPlaybook,
			Timeline: agentexec.TimelineEditingPlaybook, TalkingHead: agentexec.TalkingHeadPlaybook,
		},
	}
	for _, spec := range service.tools.Specs(true) {
		if spec.Exposure != rushestools.ExposureLLM {
			continue
		}
		info, err := spec.Implementation.Info(t.Context())
		if err != nil {
			t.Fatalf("%s info: %v", spec.Name, err)
		}
		parameters, err := info.ToJSONSchema()
		if err != nil {
			t.Fatalf("%s schema: %v", spec.Name, err)
		}
		contract.Tools = append(contract.Tools, modelContractTool{
			Name: spec.Name, Description: spec.Description, Parameters: parameters,
		})
	}
	actual, err := json.MarshalIndent(contract, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	actual = append(actual, '\n')
	path := filepath.Join("testdata", "model_contract.golden.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, actual, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v；需要重录时运行 UPDATE_GOLDEN=1 go test ./internal/agent -run TestModelContractGolden", path, err)
	}
	if !bytes.Equal(actual, want) {
		t.Fatalf("模型合同 golden 漂移；确认工具 schema/描述与 prompt 变更后运行 UPDATE_GOLDEN=1 go test ./internal/agent -run TestModelContractGolden")
	}
}
