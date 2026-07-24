package agentexec

import "strings"

const AudioTrackPlaybook = `【音频分轨】
把持续音乐与短时点缀视为两种并行职责：前者保持在音乐底轨，后者叠加到音效轨；不能把点缀接在音乐尾部冒充连续配乐。`

const BeatEditingPlaybook = `【卡点工作流】
先取得本轮音乐的节拍与完整动态证据，再按创作意图取得可核验镜头；镜头顺序、拍点和时长由你自主规划，并交给卡点重剪高层能力一次完成，不要求用户审批首剪表。拍点强弱只是声音证据，不直接代表高潮或剪法。高层调用失败时依据返回的新证据修正同一路径，不改用初版组装或成串低层编辑。`

const TimelineEditingPlaybook = `【时间线编辑】
选择或修改片段前先读取现有轨道与稳定片段标识；仅做校验、渲染或状态查询时直接执行对应目标。首次建立初版时间线时根据用户目标和素材证据自主决定片段顺序、源区间、目标时长与取舍，直接组装可回滚的初版。普通编辑只使用 timeline.insert、timeline.delete、timeline.update、timeline.split；一次调用只提交一个 kind 和一个目标或连续范围。多个独立目标按稳定顺序分别调用，每次成功产生一个可 Rewind 版本；若后一步依赖新 ID 或前一步令旧目标失效，先读取最新时间线，不得猜测。禁止提交 ops[] 或把多个目标塞进同一调用。全部编辑完成后执行 timeline.check；结构合法不能单独证明卡点已完成。`

const TalkingHeadPlaybook = `【口播工作流】
已有时间线时先并行读取 timeline.inspect、speech.search 与已有 shot.search 证据；尚无时间线时先选主讲素材建立初版。需要精确剪词时让 speech.search 返回 word_id 和源帧。相似台词、句内重说、气口和残句都只是证据：你必须结合上下文明确选择删哪一侧或保留，不向用户逐项审批可逆首剪。
把选定的 source frame 区间映射到 timeline.inspect 返回的当前主视觉片段；每次只用 timeline.delete 删除一个连续时间线范围。来自同一快照的多个独立范围按时间线从后向前提交，避免前一次波纹删除移动后续坐标；若区间跨片段、依赖新 ID 或前一步改变了映射，先重新读取时间线再继续，不得猜目标。失败只修正失败的那一个原子操作，不得重跑已成功删除。
台词清理完成后重新读取最新时间线，再按保留台词意图取得可验证 B-roll 镜头。镜头索引缺失时先并行 detect，检索池不完整时如实说明，不得编造 shot 或台词锚点。用 shot.search 的 asset_id/source range 调用 timeline.insert，只插入一段 visual_overlay；每段至少 1.5 秒，并用 timeline.update 为该段设置约 7 帧淡入淡出。不得在删除前预放 B-roll，也不得让工具自动选择镜头、改写 preserve/remove 决定或顺便执行第二种创作编辑。
最后调用 timeline.check，依据 speech_quality 中的残留气口、过短保留孤岛、未遮盖硬接缝与过短 B-roll 逐项收敛。未遮盖硬接缝优先用与当前保留台词相符的 B-roll 覆盖；没有合适画面时才作为有意跳切保留并说明。结构合法不代表语义清理完成。`

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
