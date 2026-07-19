package media

import (
	"reflect"
	"regexp"
	"testing"
)

func TestCombineDetectionMethods(t *testing.T) {
	cases := []struct {
		left, right, want string
	}{
		{"", "rms_breath", "rms_breath"},                                                  // 左空取右
		{"rms_silence", "", "rms_silence"},                                                // 右空取左
		{"rms_silence", "rms_silence", "rms_silence"},                                     // 相同不重复
		{"rms_silence", "rms_breath", "rms_breath+rms_silence"},                           // 不同则拼接
		{"rms_silence+rms_breath", "rms_breath", "rms_breath+rms_silence"},                // 已含则不再拼
		{"rms_silence+rms_breath", "rms_lipsmack", "rms_breath+rms_lipsmack+rms_silence"}, // 追加第三种
	}
	for _, tc := range cases {
		if got := combineDetectionMethods(tc.left, tc.right); got != tc.want {
			t.Errorf("combineDetectionMethods(%q,%q)=%q, want %q", tc.left, tc.right, got, tc.want)
		}
	}
}

func TestMergePausesWithBreath(t *testing.T) {
	// 呼吸段与静音段：相邻(≤gap)合并、扩展删除范围、method 合并；不相邻则各自独立。
	silence := []SpeechPause{
		{SourceStartFrame: 10, SourceEndFrame: 20, DeleteStartFrame: 12, DeleteEndFrame: 18, Method: "rms_silence"},
		{SourceStartFrame: 60, SourceEndFrame: 70, DeleteStartFrame: 62, DeleteEndFrame: 68, Method: "rms_silence"},
	}
	// 呼吸段 21..26 与第一个静音相邻(gap=2, 21<=20+2)→合并; 40..48 独立; 90..95 独立。
	breath := [][2]int{{21, 26}, {40, 48}, {90, 95}}
	got := mergePausesWithBreath(silence, breath, 2)

	want := []SpeechPause{
		// 静音1 + 呼吸21..26 合并：源扩到 26、删边扩到 [12,26]、method 拼接。
		{SourceStartFrame: 10, SourceEndFrame: 26, DeleteStartFrame: 12, DeleteEndFrame: 26, Method: "rms_breath+rms_silence"},
		{SourceStartFrame: 40, SourceEndFrame: 48, DeleteStartFrame: 40, DeleteEndFrame: 48, Method: "rms_breath"},
		{SourceStartFrame: 60, SourceEndFrame: 70, DeleteStartFrame: 62, DeleteEndFrame: 68, Method: "rms_silence"},
		{SourceStartFrame: 90, SourceEndFrame: 95, DeleteStartFrame: 90, DeleteEndFrame: 95, Method: "rms_breath"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergePausesWithBreath 结果不符\n got=%+v\nwant=%+v", got, want)
	}
}

func TestMergePausesWithBreathEmpty(t *testing.T) {
	// 无呼吸段时原样返回；无静音只有呼吸时全转成 rms_breath。
	silence := []SpeechPause{{SourceStartFrame: 5, SourceEndFrame: 9, DeleteStartFrame: 5, DeleteEndFrame: 9, Method: "rms_silence"}}
	if got := mergePausesWithBreath(silence, nil, 2); !reflect.DeepEqual(got, silence) {
		t.Errorf("空呼吸应原样返回, got=%+v", got)
	}
	onlyBreath := mergePausesWithBreath(nil, [][2]int{{3, 7}}, 2)
	want := []SpeechPause{{SourceStartFrame: 3, SourceEndFrame: 7, DeleteStartFrame: 3, DeleteEndFrame: 7, Method: "rms_breath"}}
	if !reflect.DeepEqual(onlyBreath, want) {
		t.Errorf("纯呼吸应转 rms_breath, got=%+v", onlyBreath)
	}
}

func TestMergeFrameFlags(t *testing.T) {
	mkFlags := func(n int, on ...[2]int) []bool {
		f := make([]bool, n)
		for _, r := range on {
			for i := r[0]; i < r[1]; i++ {
				f[i] = true
			}
		}
		return f
	}
	// 帧 [2,6) 与 [8,11) 之间空 2 帧(gap=2 容忍) → 合并为 [2,11)；其后 3+ 空帧才收段。
	if got := mergeFrameFlags(mkFlags(20, [2]int{2, 6}, [2]int{8, 11}), 3, 2); !reflect.DeepEqual(got, [][2]int{{2, 11}}) {
		t.Fatalf("gap<=2 应合并成 [2,11), got=%v", got)
	}
	// 大空隙(>=3 帧)收段；短段(<minFrames)被丢弃，只留合格段。
	if got := mergeFrameFlags(mkFlags(20, [2]int{1, 3}, [2]int{10, 15}), 3, 2); !reflect.DeepEqual(got, [][2]int{{10, 15}}) {
		t.Fatalf("短段应丢弃、大空隙应收段, got=%v", got)
	}
	// 末尾未闭合的段刷到 len(flags)。
	if got := mergeFrameFlags(mkFlags(12, [2]int{8, 12}), 3, 2); !reflect.DeepEqual(got, [][2]int{{8, 12}}) {
		t.Fatalf("尾段应刷到末尾, got=%v", got)
	}
	// 空输入。
	if got := mergeFrameFlags(nil, 3, 2); len(got) != 0 {
		t.Errorf("空 flags 应返回空, got=%v", got)
	}
}

func TestMatchFloat(t *testing.T) {
	pat := regexp.MustCompile(`v=(-?[0-9.]+|-?inf|nan)`)
	if v, ok := matchFloat(pat, []byte("v=-23.5")); !ok || v != -23.5 {
		t.Errorf("正常浮点解析失败: v=%v ok=%v", v, ok)
	}
	if _, ok := matchFloat(pat, []byte("nothing here")); ok {
		t.Error("无匹配应返回 false")
	}
	for _, bad := range []string{"v=-inf", "v=inf", "v=nan"} {
		if _, ok := matchFloat(pat, []byte(bad)); ok {
			t.Errorf("%q 应视为无效返回 false", bad)
		}
	}
	// 捕获到非法浮点(多个小数点)→ParseFloat 失败分支。
	if _, ok := matchFloat(pat, []byte("v=1.2.3")); ok {
		t.Error("非法浮点应返回 false")
	}
}

func TestCombineDetectionMethodsOrderIndependent(t *testing.T) {
	// 规范形与输入顺序无关：silence+breath 与 breath+silence 结果一致。
	if combineDetectionMethods("rms_silence", "rms_breath") != combineDetectionMethods("rms_breath", "rms_silence") {
		t.Error("合并应与输入顺序无关")
	}
}

// TestMergePausesWithBreathBreathFirst 覆盖 breath-first 顺序：呼吸段起点在静音段之前、
// 二者相邻合并，仍产出排序去重的规范 method。
func TestMergePausesWithBreathBreathFirst(t *testing.T) {
	silence := []SpeechPause{
		{SourceStartFrame: 60, SourceEndFrame: 70, DeleteStartFrame: 62, DeleteEndFrame: 68, Method: "rms_silence"},
	}
	// 呼吸段 54..59 排在静音段之前，且相邻(gap=2, 60<=59+2)→合并。
	got := mergePausesWithBreath(silence, [][2]int{{54, 59}}, 2)
	want := []SpeechPause{
		{SourceStartFrame: 54, SourceEndFrame: 70, DeleteStartFrame: 54, DeleteEndFrame: 68, Method: "rms_breath+rms_silence"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("breath-first 合并结果不符\n got=%+v\nwant=%+v", got, want)
	}
}

func TestDropBoundaryRanges(t *testing.T) {
	// 触及首(start==0)与尾(end>=durationFrames)的呼吸段被丢弃，只留中间段。
	got := dropBoundaryRanges([][2]int{{0, 5}, {10, 20}, {55, 60}}, 60)
	if !reflect.DeepEqual(got, [][2]int{{10, 20}}) {
		t.Fatalf("dropBoundaryRanges=%v, want [[10 20]]", got)
	}
	if got := dropBoundaryRanges(nil, 60); len(got) != 0 {
		t.Errorf("空输入应返回空, got=%v", got)
	}
}
