package agentexec

import "strings"

const AudioTrackPlaybook = `【音频分轨】
把持续音乐与短时点缀视为两种并行职责：前者保持在音乐底轨，后者叠加到音效轨；不能把点缀接在音乐尾部冒充连续配乐。`

const BeatEditingPlaybook = `【卡点工作流】
先并行取得 audio.analyze_beats 的完整拍点/动态证据与 shot.search 的可核验镜头；拍点强弱只是声音事实，不直接代表高潮或剪法。你必须自主选择镜头顺序、每个 cut frame 和精确 source range，不要求用户审批可逆首剪表。
空时间线先按选定顺序逐次 timeline.insert 主视觉片段，让每段结束帧落在明确选择的 beat frame；已有时间线用单目标 insert/delete/update 收敛，不得要求工具自动重建完整方案。修正多个主视觉切点时必须从左到右：每次波纹编辑后读取 Harness 刷新的 CurrentTimelineView，再用新起点计算下一段的源区间。1x 播放时必须满足 source_end = source_start + target_beat - timeline_start；其他倍速必须满足 round((source_end-source_start)/playback_rate) = target_beat-timeline_start。总时长优先只收敛尾段；尾素材源帧不足时，保留已验收前缀，从最早受影响切点重排剩余后缀并继续从左到右，不得回头破坏已验收切点。
主视觉总时长确定后，用 timeline.insert 单独插入 bgm，并把本轮 audio.analyze_beats 返回的完整 bpm、beat_frames、strong_beat_frames、downbeat_frames、bar_phase 与 analysis_method 原样放在 metadata 的 beat_grid 字段；SFX 作为另一次 sfx 插入，音量再用一次 timeline.update。不得让 BGM 或 SFX 混入主视觉素材，也不得自动换镜头凑时长。
每次编辑后读取 Harness 自动注入的精确版本检查证据，确认 beat_grid_present、切点覆盖与结构合同；失败只修正对应的一个镜头、音轨或参数，不重跑已成功原语。不要调用 timeline.inspect 或 timeline.check，它们由 Harness 独占。`

const TimelineEditingPlaybook = `【时间线编辑】
选择或修改片段前先读取现有轨道与稳定片段标识。首次建立初版时根据用户目标和素材证据自主决定片段顺序、源区间、目标时长与取舍：第一次 timeline.insert visual_base 自动创建 v1，后续片段逐次追加；不得改用一次接收整张 EDL 的组装工具，也不要求用户审批可逆首剪。
所有编辑只使用 timeline.insert、timeline.delete、timeline.update、timeline.split；一次调用只提交一个 kind 和一个目标或连续范围。多个独立目标按稳定顺序分别调用，每次成功产生一个可 Rewind 版本；若后一步依赖新 ID 或前一步令旧目标失效，读取下一次 provider 调用前 Harness 刷新的 CurrentTimelineView，不得猜测。禁止提交 ops[] 或把多个目标塞进同一调用。
Harness 会在每次成功写入后自动检查精确 timeline_id，并把完整证据附在同一工具结果；不要调用 timeline.inspect 或 timeline.check。要预览时从 CurrentTimelineView 读取当前 timeline_id，只调用一次 preview.generate；它会在本次调用内等待到终态并直接返回 preview_id，无需轮询或让用户发送“继续”，也不得从模型侧触发 final 导出。目标变化时使用刷新后的稳定标识，不得让渲染工具猜新目标。这个稳定标识唯一锁定当时版本，模型不要自行解析或改写。preview 完成后可在同一轮并行调用多个 preview.check，分别覆盖解码、黑帧、静帧、静音、响度或视觉语义；模型汇总证据并自行决定是否继续原子编辑，检查工具不得自动修复。`

const TalkingHeadPlaybook = `【口播工作流】
已有时间线时使用 Harness 注入的 CurrentTimelineView，并并行读取 speech.search 与已有 shot.search 证据；尚无时间线时先选主讲素材建立初版。需要精确剪词时让 speech.search 返回 word_id 和源帧。相似台词、句内重说、气口和残句都只是证据：你必须结合上下文明确选择删哪一侧或保留，不向用户逐项审批可逆首剪。
口播证据给出明确 asset_id 与 source frame 区间时，优先用 timeline.delete 的 delete_source_range，让服务端只把这个已选定的源区间确定性映射到最新时间线；映射缺失、不连续或不唯一时必须失败，不得猜替代目标。只有目标本来就是时间线范围时才用 delete_range。每次只删除一个连续范围；一次快照选出多个 timeline range 时按时间线从后向前提交，多个 source range 则分别提交，前一次波纹删除不会令后续 source 坐标失效。若依赖新 ID 或时间线目标，先重新读取时间线再继续。失败只修正失败的那一个原子操作，不得重跑已成功删除。
台词清理完成后使用刷新后的 CurrentTimelineView，再按保留台词意图取得可验证 B-roll 镜头。为某句放置 B-roll 时，必须先用 speech.search 命中该句的 asset_id 与精确 source range；删除完成后从 CurrentTimelineView 找到覆盖该 source range 的当前 A-roll clip ID，再以这个 clip ID 和原句 query/source window 重调 speech.search，直接采用返回的 timeline_start_frame，禁止自行按 clip 起点估算。检索未命中时扩大到整段素材重查，不得拿所查 clip 的起点、同义句或另一段台词代替。镜头索引缺失时先并行 detect，检索池不完整时如实说明，不得编造 shot 或台词锚点。用 shot.search 的 asset_id/source range 调用 timeline.insert，只插入一段 visual_overlay；每段至少 1.5 秒，并用 timeline.update 为该段设置约 7 帧淡入淡出。不得在删除前预放 B-roll，也不得让工具自动选择镜头、改写 preserve/remove 决定或顺便执行第二种创作编辑。
依据每次编辑工具结果中的 Harness 自动检查证据和 speech_quality，对残留气口、过短保留孤岛、未遮盖硬接缝与过短 B-roll 逐项收敛。未遮盖硬接缝优先用与当前保留台词相符的 B-roll 覆盖；没有合适画面时才作为有意跳切保留并说明。结构合法不代表语义清理完成。不要调用 timeline.inspect 或 timeline.check。`

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
