package agent

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestCoreSystemPromptStaysSmallAndContainsNoIncidentExamples(t *testing.T) {
	t.Parallel()
	if runes := utf8.RuneCountInString(coreSystemPrompt); runes > 1800 {
		t.Fatalf("core system prompt=%d runes, want <=1800", runes)
	}
	combined := strings.Join([]string{
		coreSystemPrompt, agentexec.AudioTrackPlaybook, agentexec.BeatEditingPlaybook,
		agentexec.TimelineEditingPlaybook, agentexec.TalkingHeadPlaybook,
	}, "\n")
	for _, incident := range []string{
		"压压", "跷跷", "键盘背光", "键帽", "max_pauses 提高到 100",
		"approve_content_plan", "approve_speech_cut", "approve_rough_cut",
	} {
		if strings.Contains(combined, incident) {
			t.Errorf("prompt/playbook 不应保留事故例句 %q", incident)
		}
	}
	for name, check := range map[string]struct {
		value     string
		fragments []string
	}{
		"core": {coreSystemPrompt, []string{
			"唯一客观事实", "目标明确就直接执行", "整数帧", "不可原样重试",
			"即时预览", "用户反馈可以推翻旧的节奏或镜头结论",
			"draft.content_plan", "plan.update", "RFC 7396", "不是日志或转写",
			"WorldState.user_memory", "memory.update", "本回合为准", "一次性要求不要入库", "remove_keys",
			"可逆创作细节", "Rewind", "decision_type=critical", "不得把首剪方案",
		}},
		"audio": {agentexec.AudioTrackPlaybook, []string{"持续音乐", "短时点缀", "叠加"}},
		"beat":  {agentexec.BeatEditingPlaybook, []string{"节拍与完整动态证据", "可核验镜头", "自主规划", "不要求用户审批", "卡点重剪"}},
		"timeline": {agentexec.TimelineEditingPlaybook, []string{
			"timeline.insert", "一次调用只提交一个 kind", "多个独立目标按稳定顺序分别调用",
			"禁止提交 ops[]", "自主决定", "直接组装可回滚的初版",
		}},
		"talking_head": {agentexec.TalkingHeadPlaybook, []string{
			"已有时间线", "尚无时间线", "建立初版", "逐句语音证据", "词级标识",
			"自主判断", "不向用户逐项审批",
		}},
	} {
		for _, fragment := range check.fragments {
			if !strings.Contains(check.value, fragment) {
				t.Errorf("%s prompt/playbook 丢失关键语义 %q", name, fragment)
			}
		}
	}
}

func TestTaskPlaybookSegmentsTriggerPreciselyFromCurrentWorldState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		sections map[string]any
		want     []string
	}{
		{name: "empty", sections: map[string]any{}, want: nil},
		{
			name: "audio only",
			sections: map[string]any{"assets": map[string]any{
				"audio_roles": []map[string]any{{"suggested_role": "sfx"}},
			}},
			want: []string{agentexec.AudioTrackPlaybook},
		},
		{
			name: "bgm only",
			sections: map[string]any{"assets": map[string]any{
				"material_catalog": []map[string]any{{"suggested_role": "bgm"}},
			}},
			want: []string{agentexec.BeatEditingPlaybook},
		},
		{
			name: "bgm audio role survives catalog truncation",
			sections: map[string]any{"assets": map[string]any{
				"audio_roles":      []map[string]any{{"suggested_role": "bgm"}},
				"material_catalog": []any{},
			}},
			want: []string{agentexec.AudioTrackPlaybook, agentexec.BeatEditingPlaybook},
		},
		{
			name:     "timeline only",
			sections: map[string]any{"timeline": map[string]any{}},
			want:     []string{agentexec.TimelineEditingPlaybook},
		},
		{
			name: "transcript only",
			sections: map[string]any{"assets": map[string]any{
				"material_catalog": []map[string]any{{"transcript_provider": "sidecar_srt"}},
			}},
			want: []string{agentexec.TalkingHeadPlaybook},
		},
		{
			name: "all in stable order",
			sections: map[string]any{
				"assets": map[string]any{
					"audio_roles": []map[string]any{{"asset_id": "audio_1"}},
					"material_catalog": []map[string]any{{
						"suggested_role": "bgm", "transcript_provider": "qwen_asr",
					}},
				},
				"timeline": map[string]any{},
			},
			want: []string{
				agentexec.AudioTrackPlaybook, agentexec.BeatEditingPlaybook,
				agentexec.TimelineEditingPlaybook, agentexec.TalkingHeadPlaybook,
			},
		},
		{
			name: "malformed and unrelated flags",
			sections: map[string]any{
				"assets": map[string]any{
					"audio_roles": "not-an-array",
					"material_catalog": []any{
						"bad", map[string]any{"suggested_role": "sfx", "speech_searchable": true},
					},
				},
				"timeline": nil,
			},
			want: nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			snapshot := NewWorldStateSnapshot(test.sections)
			got := agentexec.TaskPlaybookSegments(snapshot.Sections)
			if strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
				t.Fatalf("segments=%#v want=%#v", got, test.want)
			}
		})
	}
}

func TestTaskPlaybookMessageUsesCurrentSnapshotBetweenReferenceAndPatch(t *testing.T) {
	t.Parallel()
	base := NewWorldStateSnapshot(map[string]any{
		"assets": map[string]any{"material_catalog": []any{}}, "timeline": nil,
	})
	current := NewWorldStateSnapshot(map[string]any{
		"assets":   map[string]any{"material_catalog": []any{}},
		"timeline": map[string]any{"track_count": 1},
	})
	checkpoint := storage.AgentContextCheckpoint{
		WindowID: "window_jit", WindowNumber: 2, BaseSnapshotHash: "base_hash",
	}
	messages, err := renderContextMessages(
		base, current, map[string]any{"sections": map[string]any{
			"timeline": map[string]any{"track_count": 1},
		}}, checkpoint, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 ||
		messages[0].Extra["context_phase"] != "world_state_reference" ||
		messages[1].Extra["context_phase"] != "task_playbook" ||
		messages[2].Extra["context_phase"] != "world_state_update" ||
		!strings.Contains(messages[1].Content, agentexec.TimelineEditingPlaybook) {
		t.Fatalf("messages=%#v", messages)
	}

	withoutPlaybook, err := renderContextMessages(
		current, base, map[string]any{"sections": map[string]any{"timeline": nil}}, checkpoint, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(withoutPlaybook) != 2 ||
		withoutPlaybook[0].Extra["context_phase"] != "world_state_reference" ||
		withoutPlaybook[1].Extra["context_phase"] != "world_state_update" {
		t.Fatalf("base condition leaked into current playbook: %#v", withoutPlaybook)
	}
	if taskPlaybookMessage(NewWorldStateSnapshot(map[string]any{})) != nil {
		t.Fatal("empty snapshot must not emit task_playbook")
	}
}

func TestPersistentTranscriptMarksSnapshotForTalkingHeadPlaybook(t *testing.T) {
	t.Parallel()
	const draftID = "draft_prompt_transcript_trigger"
	const assetID = "asset_prompt_transcript_trigger"
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, draftID)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES(?, 'reference', '/tmp/prompt-transcript.mp4', 'video', 'local_path',
			'prompt-transcript.mp4', ?, 1, '{"duration_sec":2,"has_audio":true}',
			'ready', 'none', 1);`, assetID, assetID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO draft_asset_links(draft_id,asset_id,linked_at) VALUES(?,?,?)`,
		draftID, assetID, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{Transcripts: []reducer.TranscriptRow{{
			ID: "transcript_prompt_trigger", AssetID: assetID, ProviderID: "sidecar_srt",
			Utterances: []map[string]any{{
				"utterance_id": "utt_prompt_trigger", "source_start_frame": 0,
				"source_end_frame": 30, "text": "这是口播。",
			}},
		}}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("transcript status=%s err=%v", result.Status, err)
	}
	snapshot, err := NewContextBuilder(database).Snapshot(t.Context(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	segments := agentexec.TaskPlaybookSegments(snapshot.Sections)
	if len(segments) != 1 || segments[0] != agentexec.TalkingHeadPlaybook {
		t.Fatalf("persistent transcript segments=%#v snapshot=%#v", segments, snapshot)
	}
}

func TestMessageModifierKeepsTaskPlaybookTransientAndNonAccumulating(t *testing.T) {
	t.Parallel()
	playbook := taskPlaybookMessage(NewWorldStateSnapshot(map[string]any{
		"timeline": map[string]any{"track_count": 1},
	}))
	input := []*schema.Message{playbook, schema.UserMessage("继续编辑")}
	first := turnBudgetMessageModifier(context.Background(), input)
	second := turnBudgetMessageModifier(context.Background(), input)
	for call, messages := range [][]*schema.Message{first, second} {
		if len(messages) != 3 || messages[0].Content != coreSystemPrompt ||
			messages[1].Extra["context_phase"] != "task_playbook" ||
			messages[2].Content != "继续编辑" {
			t.Fatalf("modifier call %d messages=%#v", call+1, messages)
		}
	}
	if len(input) != 2 || input[0] != playbook || input[1].Content != "继续编辑" {
		t.Fatalf("modifier mutated state messages=%#v", input)
	}
}

func TestLLMToolDescriptionsDoNotDuplicatePromptFacts(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	sources := []string{
		coreSystemPrompt, agentexec.AudioTrackPlaybook, agentexec.BeatEditingPlaybook,
		agentexec.TimelineEditingPlaybook, agentexec.TalkingHeadPlaybook,
	}
	for _, spec := range service.tools.Specs(true) {
		if spec.Exposure != rushestools.ExposureLLM {
			continue
		}
		for _, source := range sources {
			if duplicate := commonRuneSubstringAtLeast(spec.Description, source, 15); duplicate != "" {
				t.Errorf("%s Description 与 prompt/playbook 重复 >=15 runes: %q", spec.Name, duplicate)
			}
		}
	}
}

func commonRuneSubstringAtLeast(left, right string, minimum int) string {
	leftRunes := []rune(strings.Join(strings.Fields(left), ""))
	right = strings.Join(strings.Fields(right), "")
	if minimum <= 0 || len(leftRunes) < minimum {
		return ""
	}
	for start := 0; start+minimum <= len(leftRunes); start++ {
		candidate := string(leftRunes[start : start+minimum])
		if strings.Contains(right, candidate) {
			return candidate
		}
	}
	return ""
}
