package agentexec

import "strings"

const AudioTrackPlaybook = `【音频分轨】
把持续音乐与短时点缀视为两种并行职责：前者保持在音乐底轨，后者叠加到音效轨；不能把点缀接在音乐尾部冒充连续配乐。`

const BeatEditingPlaybook = `【卡点工作流】
先取得本轮音乐的节拍与完整动态证据，再按创作意图取得可核验镜头；镜头顺序、拍点和时长由你自主规划，并交给卡点重剪高层能力一次完成，不要求用户审批首剪表。拍点强弱只是声音证据，不直接代表高潮或剪法。高层调用失败时依据返回的新证据修正同一路径，不改用初版组装或成串低层编辑。`

const TimelineEditingPlaybook = `【时间线编辑】
选择或修改片段前先读取现有轨道与稳定片段标识；仅做校验、渲染或状态查询时直接执行对应目标。首次建立初版时间线时根据用户目标和素材证据自主决定片段顺序、源区间、目标时长与取舍，直接组装可回滚的初版。普通编辑涉及两个或更多片段时，合成一个原子批次；整体替换也把新增与移除放进该批次。提交后检查结构和节奏诊断，结构合法不能单独证明卡点已完成。`

const TalkingHeadPlaybook = `【口播工作流】
已有时间线时先确认当前主讲片段；尚无时间线时先按素材角色选择主讲视频并建立初版，再读取逐句语音证据。需要精确剪词时继续读取词级标识。结合完整上下文自主判断停顿、重复、残句和 B-roll 覆盖，不向用户逐项审批可逆的保留/删除清单。配画面前按台词意图取得可验证镜头，语义尚未就绪就先理解再重搜；镜头检索若提示仍有较多素材尚未理解、不在检索池内，先批量理解补全再挑镜头，或如实告知用户当前画面检索并不完整，不要拿残缺检索池的首位结果搪塞。短镜头可锚定编辑后仍保留的连续原文，不必硬盖整句。最后把内容决定与覆盖锚点交给口播高层编辑一次执行；失败时保留用户要求，并按返回的具体关系修正。
删剪被孤岛防护拒绝时，按工具返回的合并删除建议直接删掉那段碎片，或撤回它两侧的删除，不要把删除缩到刚好过阈值来绕过。编辑成功后 speech_quality 会列出残留气口、过短保留孤岛、未遮盖硬接缝与过短 B-roll，据此自主收敛到干净；可带明确理由保留个别气口，但不得无视清单。未遮盖硬接缝是删句后两段主讲直接跳切留下的视觉跳变：优先在接缝上覆盖一段与台词相符的 B-roll 把它藏掉，没有合适画面时才作为有意跳切保留并向用户说明。B-roll 是电影语言而不是填空：每段至少 1.5 秒，放置时自动带约 7 帧视觉溶入溶出软切，A-roll 原声波纹接缝的爆音也已由渲染自动加约 12ms 微淡消除；这两类淡化都无需你手动设置，你只需决定盖不盖、盖哪一段连续台词。若结果出现 plan_drift，说明实际执行与你先前获批的删除方案有出入，必须在回复中如实告知用户保留了哪些碎片。`

// TaskPlaybookSegments 是纯函数：只读取当前 WorldState 快照的固定 section 路径，
// 按音频、卡点、时间线、口播的稳定顺序返回本轮需要的工作流段落。领域工作流知识
// 归领域包，引擎侧只负责把返回段落拼成 system 消息注入。
func TaskPlaybookSegments(sections map[string]any) []string {
	assets, _ := sections["assets"].(map[string]any)
	audioRoles := WorldStateObjectSlice(assets["audio_roles"])
	catalog := WorldStateObjectSlice(assets["material_catalog"])

	segments := make([]string, 0, 4)
	if len(audioRoles) > 0 {
		segments = append(segments, AudioTrackPlaybook)
	}
	if worldStateCatalogContains(audioRoles, "suggested_role", "bgm") ||
		worldStateCatalogContains(catalog, "suggested_role", "bgm") {
		segments = append(segments, BeatEditingPlaybook)
	}
	if timeline, exists := sections["timeline"]; exists && timeline != nil {
		segments = append(segments, TimelineEditingPlaybook)
	}
	if worldStateCatalogHasNonEmptyString(catalog, "transcript_provider") {
		segments = append(segments, TalkingHeadPlaybook)
	}
	return segments
}

// WorldStateObjectSlice 把 WorldState 里的任意值收敛成对象切片,供领域段落选择与
// 引擎侧共用(fallback 兜底、上下文构建等)。
func WorldStateObjectSlice(value any) []map[string]any {
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
