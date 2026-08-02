package understanding

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func TestDeepShotFacetPlannerAddsOrderedFramesBeyondBaseRepresentatives(t *testing.T) {
	if got := DeepFacetsForIntent("连续旋转并检查镜头运镜和屏幕文字"); !reflect.DeepEqual(
		got, []string{"temporal_action", "camera_motion", "text_ocr"},
	) {
		t.Fatalf("facets=%#v", got)
	}
	excluded := map[int]struct{}{45: {}}
	frames := planDeepFrameNumbers(0, 90, []string{"temporal_action"}, excluded)
	want := []int{11, 23, 34, 44, 56, 68, 79}
	if !reflect.DeepEqual(frames, want) {
		t.Fatalf("action frames=%#v want=%#v", frames, want)
	}
	for index := 1; index < len(frames); index++ {
		if frames[index] <= frames[index-1] {
			t.Fatalf("新增帧未按源时间排序: %#v", frames)
		}
	}
	if got := planDeepFrameNumbers(0, 90, []string{"appearance"}, excluded); len(got) != 3 {
		t.Fatalf("外观深搜应使用三帧，got=%#v", got)
	}
	if got := planDeepFrameNumbers(0, 90, DeepFacetsForIntent("读取产品型号细节"), excluded); len(got) != 7 {
		t.Fatalf("OCR/细节深搜应使用七个高密度帧，got=%#v", got)
	}
}

func TestInspectShotDeepRejectsInvalidOrMissingEvidenceBeforePublishing(t *testing.T) {
	paths, err := storage.NewPaths(t.TempDir())
	if err != nil || paths.Initialize() != nil {
		t.Fatalf("paths err=%v", err)
	}
	validRequest := DeepShotAnalysisRequest{
		ShotID: "shot", SourceStartFrame: 0, SourceEndFrame: 30,
		BoundaryVersion: 1, Facets: []string{"appearance"},
	}
	if _, err := NewAnalyzer(nil).InspectShotDeep(t.Context(), paths, "missing.mp4", validRequest); err == nil {
		t.Fatal("缺少视觉 provider 时不应执行深搜")
	}
	analyzer := NewAnalyzer(&scriptedVisionModel{responses: []string{`{"observations":[]}`}})
	invalidBoundary := validRequest
	invalidBoundary.SourceEndFrame = 0
	if _, err := analyzer.InspectShotDeep(t.Context(), paths, "missing.mp4", invalidBoundary); err == nil {
		t.Fatal("非法权威边界不应进入抽帧")
	}
	missingFacet := validRequest
	missingFacet.Facets = []string{"invented"}
	if _, err := analyzer.InspectShotDeep(t.Context(), paths, "missing.mp4", missingFacet); err == nil {
		t.Fatal("未知 facet 不应执行")
	}
	if _, err := analyzer.InspectShotDeep(t.Context(), paths, "missing.mp4", validRequest); err == nil {
		t.Fatal("既无新增帧也无可复用帧时不应调用 VLM")
	}
	missingObject := validRequest
	missingObject.ReusableFrames = []DeepFrameManifest{{
		FrameID: "f1", SourceFrame: 10, ObjectHash: strings.Repeat("a", 64), ObjectSize: 1,
	}}
	if _, err := analyzer.InspectShotDeep(t.Context(), paths, "missing.mp4", missingObject); err == nil ||
		!strings.Contains(err.Error(), "读取深搜帧") {
		t.Fatalf("缺失复用帧对象 err=%v", err)
	}
	blocked := map[int]struct{}{0: {}, 1: {}, 2: {}}
	if frames := planDeepFrameNumbers(0, 3, []string{"appearance"}, blocked); len(frames) != 0 {
		t.Fatalf("全被排除的短镜头仍规划出帧: %#v", frames)
	}
}

func TestDeepShotPayloadRequiresObjectiveObservationForEveryFacet(t *testing.T) {
	request := DeepShotAnalysisRequest{
		Facets: []string{"temporal_action", "camera_motion"},
	}
	samples := []deepFrameSample{{manifest: DeepFrameManifest{FrameID: "f1"}}}
	payload := deepShotPayload{Observations: []DeepObservation{{
		Facet: "temporal_action", Statement: "人物持续转身", FrameIDs: []string{"f1"},
	}}}
	if err := validateDeepShotPayload(payload, request, samples); err == nil {
		t.Fatal("缺少 camera_motion 通用事实时不应发布渐进式 facet 覆盖")
	}
	payload.Observations = append(payload.Observations, DeepObservation{
		Facet: "camera_motion", Statement: "镜头保持固定", FrameIDs: []string{"f1"},
	})
	if err := validateDeepShotPayload(payload, request, samples); err != nil {
		t.Fatalf("完整 objective observations 被拒绝: %v", err)
	}
	payload.Observations = append(payload.Observations, DeepObservation{
		Facet: "appearance", Statement: "人物穿着红色上衣", FrameIDs: []string{"f1"},
	})
	if err := validateDeepShotPayload(payload, request, samples); err != nil {
		t.Fatalf("合法的额外通用 facet 被拒绝: %v", err)
	}
	if got, want := deepObservationFacets(request.Facets, payload.Observations),
		[]string{"appearance", "temporal_action", "camera_motion"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("progressive facets=%#v want=%#v", got, want)
	}
}

func TestDeepShotPayloadRejectsUnboundOrMalformedEvidence(t *testing.T) {
	request := DeepShotAnalysisRequest{
		Facets: []string{"appearance"}, Requirements: []string{"人物可见"},
	}
	samples := []deepFrameSample{{manifest: DeepFrameManifest{FrameID: "f1"}}}
	valid := func() deepShotPayload {
		return deepShotPayload{
			Observations: []DeepObservation{{
				Facet: "appearance", Statement: "人物可见", FrameIDs: []string{"f1"},
			}},
			Requirements: []deepCriterionPayload{{
				ID: "r0", Status: "observed", Observation: "证据帧可见人物", FrameIDs: []string{"f1"},
			}},
		}
	}
	mutations := map[string]func(*deepShotPayload){
		"too many observations": func(value *deepShotPayload) {
			value.Observations = make([]DeepObservation, 25)
		},
		"unknown observation facet": func(value *deepShotPayload) {
			value.Observations[0].Facet = "invented"
		},
		"long observation": func(value *deepShotPayload) {
			value.Observations[0].Statement = strings.Repeat("长", 601)
		},
		"unknown observation frame": func(value *deepShotPayload) {
			value.Observations[0].FrameIDs = []string{"missing"}
		},
		"duplicate observation frame": func(value *deepShotPayload) {
			value.Observations[0].FrameIDs = []string{"f1", "f1"}
		},
		"criterion count": func(value *deepShotPayload) {
			value.Requirements = nil
		},
		"duplicate criterion": func(value *deepShotPayload) {
			value.Requirements = append(value.Requirements, value.Requirements[0])
		},
		"invalid status": func(value *deepShotPayload) {
			value.Requirements[0].Status = "maybe"
		},
		"missing criterion observation": func(value *deepShotPayload) {
			value.Requirements[0].Observation = ""
		},
		"long criterion observation": func(value *deepShotPayload) {
			value.Requirements[0].Observation = strings.Repeat("长", 601)
		},
		"missing criterion frame": func(value *deepShotPayload) {
			value.Requirements[0].FrameIDs = nil
		},
		"wrong criterion id": func(value *deepShotPayload) {
			value.Requirements[0].ID = "r1"
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			localRequest := request
			localRequest.Requirements = []string{"人物可见"}
			payload := valid()
			mutate(&payload)
			if name == "duplicate criterion" {
				localRequest.Requirements = []string{"人物可见", "第二项"}
			}
			if err := validateDeepShotPayload(payload, localRequest, samples); err == nil {
				t.Fatalf("invalid payload accepted: %#v", payload)
			}
		})
	}
}
