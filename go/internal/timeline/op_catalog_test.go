package timeline

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestCatalogCoversAllApplyPatchKinds(t *testing.T) {
	t.Parallel()

	switchKinds := applyPatchSwitchKinds(t)
	catalogKinds := make(map[string]struct{}, len(Catalog))
	if len(Catalog) != 19 {
		t.Fatalf("Catalog 条目数 = %d，期望 19", len(Catalog))
	}
	for _, spec := range Catalog {
		if _, duplicate := catalogKinds[spec.Kind]; duplicate {
			t.Fatalf("Catalog 含重复 kind %q", spec.Kind)
		}
		catalogKinds[spec.Kind] = struct{}{}
	}

	for kind := range catalogKinds {
		if _, exists := switchKinds[kind]; !exists {
			t.Errorf("Catalog kind %q 未出现在 ApplyPatch switch", kind)
		}
	}
	for kind := range switchKinds {
		if _, exists := catalogKinds[kind]; !exists {
			t.Errorf("ApplyPatch switch kind %q 未登记到 Catalog", kind)
		}
	}
}

func TestCatalogSpecsAndCorrectExamples(t *testing.T) {
	t.Parallel()

	knownTypes := map[OpFieldType]struct{}{
		OpFieldString: {}, OpFieldInteger: {}, OpFieldNumber: {},
		OpFieldBoolean: {}, OpFieldObject: {}, OpFieldStringArray: {},
	}
	for index := range Catalog {
		spec := &Catalog[index]
		t.Run(spec.Kind, func(t *testing.T) {
			if strings.TrimSpace(spec.Kind) == "" || strings.TrimSpace(spec.Summary) == "" {
				t.Fatalf("目录项缺少 kind 或 Summary: %+v", spec)
			}
			lookedUp, exists := LookupOpSpec(spec.Kind)
			if !exists || lookedUp != spec {
				t.Fatalf("LookupOpSpec(%q) = (%p,%v)，期望 %p,true", spec.Kind, lookedUp, exists, spec)
			}

			fieldNames := map[string]struct{}{"kind": {}}
			for _, field := range spec.Fields {
				if strings.TrimSpace(field.Name) == "" || strings.TrimSpace(field.Desc) == "" {
					t.Fatalf("字段缺少 Name 或 Desc: %+v", field)
				}
				if _, exists := knownTypes[field.Type]; !exists {
					t.Fatalf("字段 %s 使用未知类型 %q", field.Name, field.Type)
				}
				for _, name := range append([]string{field.Name}, field.Aliases...) {
					if _, duplicate := fieldNames[name]; duplicate {
						t.Fatalf("字段名或别名 %q 重复", name)
					}
					fieldNames[name] = struct{}{}
				}
			}
			for _, name := range spec.RequireAny {
				if _, exists := fieldNames[name]; !exists {
					t.Errorf("RequireAny 字段 %q 未登记到 Fields", name)
				}
			}

			example := CorrectOpExample(*spec)
			if err := ValidateOpFields(example); err != nil {
				t.Fatalf("CorrectOpExample 未通过字段校验: example=%#v err=%v", example, err)
			}
			for _, field := range spec.Fields {
				if field.Injected {
					if _, leaked := example[field.Name]; leaked {
						t.Errorf("服务端注入字段 %s 泄漏到模型示例", field.Name)
					}
				}
			}
			withInjected := CorrectOpExample(*spec)
			for _, field := range spec.Fields {
				if field.Example == nil {
					t.Errorf("字段 %s 缺少 Example", field.Name)
					continue
				}
				if field.Injected {
					withInjected[field.Name] = cloneOpExampleValue(field.Example)
				}
			}
			if err := ValidateOpFields(withInjected); err != nil {
				t.Errorf("含注入字段的完整 Example 未通过校验: example=%#v err=%v", withInjected, err)
			}
		})
	}
	if _, exists := LookupOpSpec("not_an_op"); exists {
		t.Fatal("LookupOpSpec 对未知 kind 返回 exists=true")
	}
}

func TestValidateOpFieldsAliasesAndCompatibility(t *testing.T) {
	t.Parallel()

	valid := []struct {
		name string
		op   map[string]any
	}{
		{
			name: "canonical clip id",
			op: map[string]any{
				"kind": "trim_clip_edge", "timeline_clip_id": "clip_1", "timeline_frame": 10, "edge": "end",
			},
		},
		{
			name: "clip_id alias",
			op: map[string]any{
				"kind": "trim_clip_edge", "clip_id": "clip_1", "timeline_frame": 10, "edge": "end",
			},
		},
		{
			name: "integral float32 frame",
			op: map[string]any{
				"kind": "delete_range", "start_frame": float32(1), "end_frame": float32(2),
			},
		},
		{
			name: "integral float64 frame from json",
			op: map[string]any{
				"kind": "delete_range", "start_frame": float64(1), "end_frame": float64(2),
			},
		},
		{
			name: "int64 frame",
			op: map[string]any{
				"kind": "delete_range", "start_frame": int64(1), "end_frame": int64(2),
			},
		},
		{
			name: "number variants",
			op: map[string]any{
				"kind": "set_playback_rate", "clip_id": "clip_1", "playback_rate": int64(2),
			},
		},
		{
			name: "float32 number",
			op: map[string]any{
				"kind": "adjust_gain", "clip_id": "clip_1", "gain_db": float32(-3.5),
			},
		},
		{
			name: "injected fields may be absent",
			op:   map[string]any{"kind": "sync_original_audio"},
		},
		{
			name: "json string array",
			op: map[string]any{
				"kind": "sync_original_audio", "audio_asset_ids": []any{"asset_1", "asset_2"},
			},
		},
		{
			name: "subtitle style only",
			op: map[string]any{
				"kind": "edit_subtitle_text", "timeline_clip_id": "subtitle_1", "style": "large_center",
			},
		},
		{
			name: "unknown compatibility field is tolerated",
			op: map[string]any{
				"kind": "delete_clip", "clip_id": "clip_1", "future_metadata": map[string]any{"v": 1},
			},
		},
	}
	for _, testCase := range valid {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateOpFields(testCase.op); err != nil {
				t.Fatalf("ValidateOpFields(%#v) = %v", testCase.op, err)
			}
		})
	}
}

func TestValidateOpFieldsReturnsTypedErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		op         map[string]any
		wantKind   string
		wantField  string
		wantSpec   bool
		wantReason string
	}{
		{name: "missing kind", op: map[string]any{}, wantField: "kind", wantReason: "缺少必填字段"},
		{name: "kind wrong type", op: map[string]any{"kind": 1}, wantField: "kind", wantReason: "非空字符串"},
		{name: "unknown kind", op: map[string]any{"kind": "remove_clip"}, wantKind: "remove_clip", wantField: "kind", wantReason: "受支持"},
		{
			name: "trim edge uses wrong position field",
			op: map[string]any{
				"kind": "trim_clip_edge", "timeline_clip_id": "clip_1", "target_frame": 10, "edge": "end",
			},
			wantKind: "trim_clip_edge", wantField: "timeline_frame", wantSpec: true, wantReason: "缺少必填字段",
		},
		{
			name: "required field wrong type",
			op: map[string]any{
				"kind": "delete_range", "start_frame": "1", "end_frame": 2,
			},
			wantKind: "delete_range", wantField: "start_frame", wantSpec: true, wantReason: "整数帧",
		},
		{
			name:     "alias wrong type",
			op:       map[string]any{"kind": "delete_clip", "clip_id": 1},
			wantKind: "delete_clip", wantField: "clip_id", wantSpec: true, wantReason: "字符串",
		},
		{
			name: "optional field wrong type",
			op: map[string]any{
				"kind": "move_clip", "clip_id": "clip_1", "target_frame": 10, "mode": true,
			},
			wantKind: "move_clip", wantField: "mode", wantSpec: true, wantReason: "字符串",
		},
		{
			name:     "set track state needs one update",
			op:       map[string]any{"kind": "set_track_state", "track_id": "bgm"},
			wantKind: "set_track_state", wantField: "muted", wantSpec: true, wantReason: "至少需要提供一个",
		},
		{
			name:     "subtitle edit needs text or style",
			op:       map[string]any{"kind": "edit_subtitle_text", "timeline_clip_id": "subtitle_1"},
			wantKind: "edit_subtitle_text", wantField: "text", wantSpec: true, wantReason: "至少需要提供一个",
		},
		{
			name:     "boolean wrong type",
			op:       map[string]any{"kind": "set_clip_linked", "clip_id": "clip_1", "linked": "true"},
			wantKind: "set_clip_linked", wantField: "linked", wantSpec: true, wantReason: "布尔值",
		},
		{
			name: "object wrong type",
			op: map[string]any{
				"kind": "insert_clip", "asset_id": "asset_1", "source_start_frame": 0, "source_end_frame": 10,
				"metadata": []any{"not", "an", "object"},
			},
			wantKind: "insert_clip", wantField: "metadata", wantSpec: true, wantReason: "对象",
		},
		{
			name:     "string array wrong member type",
			op:       map[string]any{"kind": "sync_original_audio", "audio_asset_ids": []any{"asset_1", 2}},
			wantKind: "sync_original_audio", wantField: "audio_asset_ids", wantSpec: true, wantReason: "字符串数组",
		},
		{
			name:     "fractional frame",
			op:       map[string]any{"kind": "delete_range", "start_frame": 1.5, "end_frame": 2},
			wantKind: "delete_range", wantField: "start_frame", wantSpec: true, wantReason: "整数帧",
		},
		{
			name:     "non finite frame",
			op:       map[string]any{"kind": "delete_range", "start_frame": math.Inf(1), "end_frame": 2},
			wantKind: "delete_range", wantField: "start_frame", wantSpec: true, wantReason: "整数帧",
		},
		{
			name:     "non finite number",
			op:       map[string]any{"kind": "adjust_gain", "clip_id": "clip_1", "gain_db": math.NaN()},
			wantKind: "adjust_gain", wantField: "gain_db", wantSpec: true, wantReason: "有限数值",
		},
		{
			name:     "number wrong type",
			op:       map[string]any{"kind": "adjust_gain", "clip_id": "clip_1", "gain_db": "-3"},
			wantKind: "adjust_gain", wantField: "gain_db", wantSpec: true, wantReason: "有限数值",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateOpFields(testCase.op)
			if err == nil {
				t.Fatalf("ValidateOpFields(%#v) = nil", testCase.op)
			}
			var fieldErr *OpFieldError
			if !errors.As(err, &fieldErr) {
				t.Fatalf("错误类型 = %T，期望 *OpFieldError", err)
			}
			if fieldErr.Kind != testCase.wantKind || fieldErr.Field != testCase.wantField {
				t.Errorf("错误定位 = (%q,%q)，期望 (%q,%q)", fieldErr.Kind, fieldErr.Field, testCase.wantKind, testCase.wantField)
			}
			if (fieldErr.Spec != nil) != testCase.wantSpec {
				t.Errorf("Spec 是否存在 = %v，期望 %v", fieldErr.Spec != nil, testCase.wantSpec)
			}
			if fieldErr.Spec != nil && fieldErr.Spec.Kind != testCase.wantKind {
				t.Errorf("Spec.Kind = %q，期望 %q", fieldErr.Spec.Kind, testCase.wantKind)
			}
			if !strings.Contains(fieldErr.Error(), testCase.wantReason) {
				t.Errorf("Error() = %q，期望包含 %q", fieldErr.Error(), testCase.wantReason)
			}
		})
	}
}

func TestValidateOpFieldsDoesNotMutateInputAndErrorFormattingIsSafe(t *testing.T) {
	t.Parallel()
	op := map[string]any{
		"kind": "insert_clip", "asset_id": "asset_1", "source_start_frame": float64(0),
		"source_end_frame": float64(30), "metadata": map[string]any{"nested": []any{"value"}},
	}
	want := cloneOpExampleValue(op).(map[string]any)
	if err := ValidateOpFields(op); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(op, want) {
		t.Fatalf("ValidateOpFields mutated input: got=%#v want=%#v", op, want)
	}
	if (*OpFieldError)(nil).Error() != "" ||
		(&OpFieldError{Reason: "无效"}).Error() != "时间线补丁无效" ||
		(&OpFieldError{Kind: "custom", Reason: "无效"}).Error() != "时间线补丁 custom 无效" {
		t.Fatal("OpFieldError formatting mismatch")
	}
	original := []any{map[string]any{"x": "y"}}
	cloned := cloneOpExampleValue(original).([]any)
	cloned[0].(map[string]any)["x"] = "changed"
	if original[0].(map[string]any)["x"] != "y" {
		t.Fatal("cloneOpExampleValue did not deep-clone []any")
	}
	if joinOpFieldNames(nil) != "" {
		t.Fatal("empty field name join mismatch")
	}
}

func TestCorrectOpExampleReturnsIndependentValues(t *testing.T) {
	t.Parallel()

	spec, exists := LookupOpSpec("insert_clip")
	if !exists {
		t.Fatal("insert_clip 未登记")
	}
	first := CorrectOpExample(*spec)
	metadata := first["metadata"].(map[string]any)
	metadata["source"] = "mutated"
	second := CorrectOpExample(*spec)
	if second["metadata"].(map[string]any)["source"] != "catalog_example" {
		t.Fatal("CorrectOpExample 返回了共享的可变示例值")
	}
}

func applyPatchSwitchKinds(t *testing.T) map[string]struct{} {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位 op_catalog_test.go")
	}
	filename := filepath.Join(filepath.Dir(currentFile), "timeline.go")
	file, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s: %v", filename, err)
	}

	var applyPatch *ast.FuncDecl
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == "ApplyPatch" {
			applyPatch = function
			break
		}
	}
	if applyPatch == nil {
		t.Fatal("timeline.go 中未找到 ApplyPatch")
	}

	var operationSwitch *ast.SwitchStmt
	for _, statement := range applyPatch.Body.List {
		switchStatement, ok := statement.(*ast.SwitchStmt)
		if !ok {
			continue
		}
		identifier, ok := switchStatement.Tag.(*ast.Ident)
		if ok && identifier.Name == "kind" {
			operationSwitch = switchStatement
			break
		}
	}
	if operationSwitch == nil {
		t.Fatal("ApplyPatch 中未找到 switch kind")
	}

	kinds := map[string]struct{}{}
	hasDefault := false
	for _, statement := range operationSwitch.Body.List {
		clause := statement.(*ast.CaseClause)
		if len(clause.List) == 0 {
			hasDefault = true
			continue
		}
		for _, expression := range clause.List {
			literal, ok := expression.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Fatalf("ApplyPatch switch 含非字符串 case: %T", expression)
			}
			kind, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatalf("解析 switch case %s: %v", literal.Value, err)
			}
			if _, duplicate := kinds[kind]; duplicate {
				t.Fatalf("ApplyPatch switch 含重复 kind %q", kind)
			}
			kinds[kind] = struct{}{}
		}
	}
	if !hasDefault {
		t.Fatal("ApplyPatch switch 缺少 default")
	}
	return kinds
}
