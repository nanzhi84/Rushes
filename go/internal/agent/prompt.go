package agent

import (
	"strings"

	"github.com/cloudwego/eino/schema"
)

// coreSystemPrompt 只承载每类任务都成立的稳定不变量。工具参数契约由
// schema/Description 负责，任务工作流则由下面的 WorldState 条件段按需注入。
const coreSystemPrompt = `你是 Rushes 本地视频剪辑 Agent，职责是实际修改当前草稿并交付结果，而不是只给建议。

上下文协议：系统消息定义能力与安全边界；最新用户消息给出当前创作意图，也可以纠正旧判断；【WorldState 参考快照】应用其后的当前增量后，才是素材、时间线、任务和错误的唯一客观事实。历史回复与压缩交接只能延续目标和决定，不能覆盖客观状态。素材目录是常驻的精简索引，不是完整镜头或转写内容。

draft.content_plan 是你的持久创作计划本，用 plan.update 维护（默认 RFC 7396 增量，reset=true 整体重写）；只记提炼后的意图与决定，不是日志或转写存放处。

通用规则：
1. 只通过已注册能力完成理解、编辑、验证、预览、质检和导出；不得编造文件、素材、时间线、任务或产物。
2. 目标明确就直接执行。只有缺失的选择会显著改变成片时才向用户提问；获得答案后自动继续原任务。
3. 精确时间坐标统一使用整数帧；编辑操作必须是带 kind 的扁平对象，禁止自行换算或传递秒字段。
4. 失败后先读 observation、recovery 和最新状态，再调整参数或补取证据；不可原样重试，也不可删除用户要求来绕过错误。恢复预算耗尽时停止调用，并如实说明已完成、未完成及下一步。
5. 浏览器编辑代理负责即时预览，普通移动、裁剪和分割不触发离线渲染。只有用户明确需要可分享预览或离线画质检查时才生成预览并质检；最终导出读取原素材。
6. 用户反馈可以推翻旧的节奏或镜头结论。应从当前状态和本轮证据继续，不复用已过期判断；除非用户明确要求，不从头重做，也不删除已有素材、时间线或已完成理解。`

const audioTrackPlaybook = `【音频分轨】
把持续音乐与短时点缀视为两种并行职责：前者保持在音乐底轨，后者叠加到音效轨；不能把点缀接在音乐尾部冒充连续配乐。`

const beatEditingPlaybook = `【卡点工作流】
先取得本轮音乐的节拍与完整动态证据，再按创作意图取得可核验镜头，最后交给卡点重剪高层能力一次完成。拍点强弱只是声音证据，不直接代表高潮或剪法。高层调用失败时依据返回的新证据修正同一路径，不改用初版组装或成串低层编辑。`

const timelineEditingPlaybook = `【时间线编辑】
选择或修改片段前先读取现有轨道与稳定片段标识；仅做校验、渲染或状态查询时直接执行对应目标。普通编辑涉及两个或更多片段时，合成一个原子批次；整体替换也把新增与移除放进该批次。提交后检查结构和节奏诊断，结构合法不能单独证明卡点已完成。`

const talkingHeadPlaybook = `【口播工作流】
已有时间线时先确认当前主讲片段；尚无时间线时先按素材角色选择主讲视频并建立初版，再读取逐句语音证据。需要精确剪词时继续读取词级标识。模型结合完整上下文判断停顿、重复和残句。配画面前按台词意图取得可验证镜头，语义尚未就绪就先理解再重搜；短镜头可锚定编辑后仍保留的连续原文，不必硬盖整句。最后把内容决定与覆盖锚点交给口播高层编辑一次执行；失败时保留用户要求，并按返回的具体关系修正。`

// taskPlaybookSegments 是纯函数：只读取当前 WorldState 的固定路径，按
// 音频、卡点、时间线、口播的稳定顺序返回本轮需要的工作流。
func taskPlaybookSegments(snapshot WorldStateSnapshot) []string {
	assets, _ := snapshot.Sections["assets"].(map[string]any)
	audioRoles := worldStateObjectSlice(assets["audio_roles"])
	catalog := worldStateObjectSlice(assets["material_catalog"])

	segments := make([]string, 0, 4)
	if len(audioRoles) > 0 {
		segments = append(segments, audioTrackPlaybook)
	}
	if worldStateCatalogContains(audioRoles, "suggested_role", "bgm") ||
		worldStateCatalogContains(catalog, "suggested_role", "bgm") {
		segments = append(segments, beatEditingPlaybook)
	}
	if timeline, exists := snapshot.Sections["timeline"]; exists && timeline != nil {
		segments = append(segments, timelineEditingPlaybook)
	}
	if worldStateCatalogHasNonEmptyString(catalog, "transcript_provider") {
		segments = append(segments, talkingHeadPlaybook)
	}
	return segments
}

func taskPlaybookMessage(snapshot WorldStateSnapshot) *schema.Message {
	segments := taskPlaybookSegments(snapshot)
	if len(segments) == 0 {
		return nil
	}
	message := schema.SystemMessage(
		"【按当前 WorldState 启用的任务工作流】\n" + strings.Join(segments, "\n\n"),
	)
	message.Extra = map[string]any{"context_phase": "task_playbook"}
	return message
}

func worldStateObjectSlice(value any) []map[string]any {
	switch typed := value.(type) {
	case []any:
		result := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			if object, ok := item.(map[string]any); ok {
				result = append(result, object)
			}
		}
		return result
	case []map[string]any:
		return typed
	default:
		return nil
	}
}

func worldStateCatalogContains(catalog []map[string]any, key, expected string) bool {
	for _, item := range catalog {
		value, _ := item[key].(string)
		if strings.TrimSpace(value) == expected {
			return true
		}
	}
	return false
}

func worldStateCatalogHasNonEmptyString(catalog []map[string]any, key string) bool {
	for _, item := range catalog {
		value, _ := item[key].(string)
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}
