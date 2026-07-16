package timeline

import (
	"fmt"
	"math"
)

// OpFieldType 是时间线补丁目录使用的、与具体 schema 库无关的字段类型。
// tools 层会在需要时把这些类型编译成面向模型的 JSON Schema。
type OpFieldType = string

const (
	OpFieldString      OpFieldType = "string"
	OpFieldInteger     OpFieldType = "integer"
	OpFieldNumber      OpFieldType = "number"
	OpFieldBoolean     OpFieldType = "boolean"
	OpFieldObject      OpFieldType = "object"
	OpFieldStringArray OpFieldType = "string_array"
)

// OpField 描述一个时间线补丁字段。Injected 字段由服务端补齐，模型侧
// schema 和纠错示例应隐藏它，但运行时仍会校验调用方显式提供的值。
type OpField struct {
	Name     string
	Aliases  []string
	Type     OpFieldType
	Required bool
	Injected bool
	Desc     string
	Example  any
}

// OpSpec 是一种时间线补丁的完整字段目录。
type OpSpec struct {
	Kind       string
	Summary    string
	Fields     []OpField
	RequireAny []string
}

// OpFieldError 表示补丁在进入具体语义处理前便可确定的字段错误。
// Spec 在 kind 已知时始终非空，供 agent 生成该 kind 的精确 JIT 修复提示；
// 未知 kind（包括 kind 缺失或类型错误）时 Spec 为 nil。
type OpFieldError struct {
	Kind   string
	Field  string
	Reason string
	Spec   *OpSpec
}

func (err *OpFieldError) Error() string {
	if err == nil {
		return ""
	}
	if err.Kind == "" {
		if err.Field == "kind" {
			return "时间线补丁 kind " + err.Reason
		}
		return "时间线补丁" + err.Reason
	}
	if err.Field == "" {
		return fmt.Sprintf("时间线补丁 %s %s", err.Kind, err.Reason)
	}
	return fmt.Sprintf("时间线补丁 %s 的字段 %s %s", err.Kind, err.Field, err.Reason)
}

// Catalog 是 ApplyPatch 支持的 19 种语义补丁的单一字段事实源。
// kind 本身由 OpSpec.Kind 表示，不重复列入 Fields。
var Catalog = []OpSpec{
	{
		Kind:    "trim_clip",
		Summary: "修改片段源区间，并按播放速率重算时长",
		Fields: []OpField{
			clipIDField(),
			field("source_start_frame", OpFieldInteger, true, "新的素材入点，使用整数帧", 0),
			field("source_end_frame", OpFieldInteger, true, "新的素材出点，须大于 source_start_frame", 150),
		},
	},
	{
		Kind:    "split_clip",
		Summary: "在片段内部的时间线帧处切分片段",
		Fields: []OpField{
			clipIDField(),
			field("split_frame", OpFieldInteger, true, "切点的时间线整数帧", 75),
			field("new_timeline_clip_id", OpFieldString, false, "可选的右侧新片段 ID", "clip_v2_002"),
		},
	},
	{
		Kind:    "reorder_clip",
		Summary: "按目标时间线位置重排主视觉片段",
		Fields: []OpField{
			clipIDField(),
			field("target_frame", OpFieldInteger, true, "目标时间线整数帧", 150),
		},
	},
	{
		Kind:    "move_clip",
		Summary: "把片段移动到目标时间线位置或兼容轨道",
		Fields: []OpField{
			clipIDField(),
			field("target_frame", OpFieldInteger, true, "目标时间线整数帧", 150),
			field("mode", OpFieldString, false, "移动模式：insert 或 overwrite，默认 insert", "insert"),
			field("target_track_id", OpFieldString, false, "目标轨道 ID，默认保持原轨道", "visual_overlay"),
		},
	},
	{
		Kind:    "trim_clip_edge",
		Summary: "按时间线帧裁剪片段起始或结束边缘",
		Fields: []OpField{
			clipIDField(),
			field("timeline_frame", OpFieldInteger, true, "裁剪点的时间线整数帧；不是 target_frame", 75),
			field("edge", OpFieldString, true, "要裁剪的边缘：start 或 end", "end"),
		},
	},
	{
		Kind:    "delete_clip",
		Summary: "删除片段及其联动组",
		Fields: []OpField{
			clipIDField(),
		},
	},
	{
		Kind:       "set_track_state",
		Summary:    "更新轨道的静音、独奏、锁定或音量状态",
		RequireAny: []string{"muted", "solo", "locked", "gain_db"},
		Fields: []OpField{
			field("track_id", OpFieldString, true, "目标轨道 ID", "bgm"),
			field("muted", OpFieldBoolean, false, "是否静音", false),
			field("solo", OpFieldBoolean, false, "是否独奏", false),
			field("locked", OpFieldBoolean, false, "是否锁定", false),
			field("gain_db", OpFieldNumber, false, "音频轨音量，范围 [-60,12] dB", 0.0),
		},
	},
	{
		Kind:    "set_track_ducking",
		Summary: "设置 BGM 在人声出现时自动闪避",
		Fields: []OpField{
			field("track_id", OpFieldString, true, "只能是 bgm 轨", "bgm"),
			field("enabled", OpFieldBoolean, true, "是否启用自动闪避", true),
			field("duck_db", OpFieldNumber, true, "人声出现时的压低量，范围 [-18,-3] dB", -9.0),
			field("trigger_tracks", OpFieldStringArray, true, "触发轨道，只能选择 voiceover 或 original_audio", []string{"voiceover", "original_audio"}),
		},
	},
	{
		Kind:    "set_clip_linked",
		Summary: "设置片段是否参与音画联动",
		Fields: []OpField{
			clipIDField(),
			field("linked", OpFieldBoolean, true, "是否启用联动", true),
		},
	},
	{
		Kind:    "insert_subtitle",
		Summary: "在字幕轨插入一条字幕",
		Fields: []OpField{
			field("start_frame", OpFieldInteger, true, "字幕开始的时间线整数帧", 0),
			field("end_frame", OpFieldInteger, true, "字幕结束的时间线整数帧", 90),
			field("text", OpFieldString, true, "非空字幕文字", "示例字幕"),
			field("style", OpFieldString, false, "字幕样式：default、large_center、top_bar、minimal 或 bold_bottom", "default"),
			field("timeline_clip_id", OpFieldString, false, "可选字幕片段 ID，缺省时自动生成", "subtitle_v2_001"),
		},
	},
	{
		Kind:    "delete_range",
		Summary: "从所有轨道波纹删除一段时间线范围",
		Fields: []OpField{
			field("start_frame", OpFieldInteger, true, "删除范围开始的时间线整数帧", 30),
			field("end_frame", OpFieldInteger, true, "删除范围结束的时间线整数帧", 60),
		},
	},
	{
		Kind:    "insert_clip",
		Summary: "把素材片段插入指定轨道",
		Fields: []OpField{
			field("asset_id", OpFieldString, true, "要插入的素材 ID", "asset_001"),
			field("source_start_frame", OpFieldInteger, true, "素材入点整数帧", 0),
			field("source_end_frame", OpFieldInteger, true, "素材出点整数帧", 150),
			field("track_id", OpFieldString, false, "目标轨道 ID，默认 visual_base", "visual_base"),
			field("timeline_start_frame", OpFieldInteger, false, "非主视觉片段的时间线起点整数帧", 0),
			field("role", OpFieldString, false, "片段角色，默认 b_roll", "b_roll"),
			field("timeline_clip_id", OpFieldString, false, "可选片段 ID，缺省时自动生成", "clip_v2_001"),
			field("parent_block_id", OpFieldString, false, "可选联动组 ID", "link_clip_v2_001"),
			field("metadata", OpFieldObject, false, "可选片段元数据对象", map[string]any{"source": "catalog_example"}),
			injectedField("asset_kind", OpFieldString, "由服务端根据 asset_id 注入的素材类型", "video"),
			injectedField("include_original_audio", OpFieldBoolean, "由服务端为主视觉视频注入的原声联动标志", true),
		},
	},
	{
		Kind:    "sync_original_audio",
		Summary: "根据最新主视觉片段原子重建原声音轨",
		Fields: []OpField{
			injectedField("audio_asset_ids", OpFieldStringArray, "由服务端注入的带音频视频素材 ID 列表", []string{"asset_001"}),
		},
	},
	{
		Kind:    "replace_clip",
		Summary: "保持片段帧范围并替换素材",
		Fields: []OpField{
			clipIDField(),
			field("asset_id", OpFieldString, true, "替换后的素材 ID", "asset_002"),
			field("role", OpFieldString, false, "可选的新片段角色", "b_roll"),
		},
	},
	{
		Kind:    "set_playback_rate",
		Summary: "设置片段播放速率并重算时长",
		Fields: []OpField{
			clipIDField(),
			field("playback_rate", OpFieldNumber, true, "播放速率，范围 (0,8]", 1.25),
		},
	},
	{
		Kind:    "adjust_gain",
		Summary: "调整片段音量",
		Fields: []OpField{
			clipIDField(),
			field("gain_db", OpFieldNumber, true, "片段音量，范围 [-60,12] dB", -3.0),
		},
	},
	{
		Kind:    "set_clip_fades",
		Summary: "设置音频或视频片段的音画淡入淡出",
		Fields: []OpField{
			clipIDField(),
			field("fade_in_frames", OpFieldInteger, true, "淡入时长整数帧", 15),
			field("fade_out_frames", OpFieldInteger, true, "淡出时长整数帧", 15),
		},
	},
	{
		Kind:       "edit_subtitle_text",
		Summary:    "修改字幕片段文字或样式",
		RequireAny: []string{"text", "style"},
		Fields: []OpField{
			clipIDField(),
			field("text", OpFieldString, false, "可选的新字幕文字；提供时必须非空", "修改后的字幕"),
			field("style", OpFieldString, false, "可选字幕样式：default、large_center、top_bar、minimal 或 bold_bottom", "large_center"),
		},
	},
	{
		Kind:    "remove_track_clips",
		Summary: "清空非主视觉且未锁定轨道的全部片段",
		Fields: []OpField{
			field("track_id", OpFieldString, true, "要清空的轨道 ID", "sfx"),
		},
	},
}

// LookupOpSpec 按 kind 返回目录项。返回的指针只应用于读取。
func LookupOpSpec(kind string) (*OpSpec, bool) {
	for index := range Catalog {
		if Catalog[index].Kind == kind {
			return &Catalog[index], true
		}
	}
	return nil, false
}

// CorrectOpExample 从目录生成字段形状正确的模型侧示例。服务端注入字段不会
// 出现在示例里；可选字段也会保留，以便一次展示该 op 的完整可用字段集合。
func CorrectOpExample(spec OpSpec) map[string]any {
	example := make(map[string]any, len(spec.Fields)+1)
	example["kind"] = spec.Kind
	for _, field := range spec.Fields {
		if field.Injected || field.Example == nil {
			continue
		}
		example[field.Name] = cloneOpExampleValue(field.Example)
	}
	return example
}

// ValidateOpFields 只负责目录可判定的字段存在性和类型检查。范围、轨道锁定、
// 音画联动等依赖当前 Document 的语义约束仍由各 ApplyPatch handler 校验。
// 为兼容程序化调用和历史客户端，这里不会拒绝目录之外的附加字段。
func ValidateOpFields(operation map[string]any) error {
	rawKind, exists := operation["kind"]
	if !exists {
		return &OpFieldError{Field: "kind", Reason: "缺少必填字段"}
	}
	kind, ok := rawKind.(string)
	if !ok || kind == "" {
		return &OpFieldError{Field: "kind", Reason: "必须是非空字符串"}
	}
	spec, exists := LookupOpSpec(kind)
	if !exists {
		return &OpFieldError{Kind: kind, Field: "kind", Reason: "不是受支持的操作类型"}
	}

	for _, field := range spec.Fields {
		names := make([]string, 0, len(field.Aliases)+1)
		names = append(names, field.Name)
		names = append(names, field.Aliases...)
		provided := false
		for _, name := range names {
			value, found := operation[name]
			if !found {
				continue
			}
			provided = true
			if !validOpFieldValue(field.Type, value) {
				return &OpFieldError{
					Kind:   kind,
					Field:  name,
					Reason: "类型必须是" + opFieldTypeDescription(field.Type),
					Spec:   spec,
				}
			}
		}
		if field.Required && !field.Injected && !provided {
			return &OpFieldError{
				Kind:   kind,
				Field:  field.Name,
				Reason: "缺少必填字段",
				Spec:   spec,
			}
		}
	}
	if len(spec.RequireAny) > 0 {
		for _, name := range spec.RequireAny {
			if _, exists := operation[name]; exists {
				return nil
			}
		}
		return &OpFieldError{
			Kind:   kind,
			Field:  spec.RequireAny[0],
			Reason: "至少需要提供一个可更新字段：" + joinOpFieldNames(spec.RequireAny),
			Spec:   spec,
		}
	}
	return nil
}

func field(name string, fieldType OpFieldType, required bool, desc string, example any) OpField {
	return OpField{Name: name, Type: fieldType, Required: required, Desc: desc, Example: example}
}

func injectedField(name string, fieldType OpFieldType, desc string, example any) OpField {
	return OpField{Name: name, Type: fieldType, Injected: true, Desc: desc, Example: example}
}

func clipIDField() OpField {
	return OpField{
		Name:     "timeline_clip_id",
		Aliases:  []string{"clip_id"},
		Type:     OpFieldString,
		Required: true,
		Desc:     "目标时间线片段 ID；兼容别名 clip_id",
		Example:  "clip_v1_001",
	}
}

func validOpFieldValue(fieldType OpFieldType, value any) bool {
	switch fieldType {
	case OpFieldString:
		_, ok := value.(string)
		return ok
	case OpFieldInteger:
		return validIntegerValue(value)
	case OpFieldNumber:
		return validNumberValue(value)
	case OpFieldBoolean:
		_, ok := value.(bool)
		return ok
	case OpFieldObject:
		_, ok := value.(map[string]any)
		return ok
	case OpFieldStringArray:
		switch typed := value.(type) {
		case []string:
			return true
		case []any:
			for _, item := range typed {
				if _, ok := item.(string); !ok {
					return false
				}
			}
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func validIntegerValue(value any) bool {
	var number float64
	switch typed := value.(type) {
	case int:
		return true
	case int64:
		return int64(int(typed)) == typed
	case float32:
		number = float64(typed)
	case float64:
		number = typed
	default:
		return false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number {
		return false
	}
	converted := int(number)
	return float64(converted) == number
}

func validNumberValue(value any) bool {
	var number float64
	switch typed := value.(type) {
	case int, int64:
		return true
	case float32:
		number = float64(typed)
	case float64:
		number = typed
	default:
		return false
	}
	return !math.IsNaN(number) && !math.IsInf(number, 0)
}

func opFieldTypeDescription(fieldType OpFieldType) string {
	switch fieldType {
	case OpFieldString:
		return "字符串"
	case OpFieldInteger:
		return "整数帧"
	case OpFieldNumber:
		return "有限数值"
	case OpFieldBoolean:
		return "布尔值"
	case OpFieldObject:
		return "对象"
	case OpFieldStringArray:
		return "字符串数组"
	default:
		return string(fieldType)
	}
}

func joinOpFieldNames(names []string) string {
	if len(names) == 0 {
		return ""
	}
	result := names[0]
	for _, name := range names[1:] {
		result += "、" + name
	}
	return result
}

func cloneOpExampleValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, item := range typed {
			cloned[key] = cloneOpExampleValue(item)
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneOpExampleValue(item)
		}
		return cloned
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}
