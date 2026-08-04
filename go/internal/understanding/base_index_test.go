package understanding

import (
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func TestBuildBaseIndexShotsRejectsIncompleteEvidence(t *testing.T) {
	valid := baseIndexTestSegment()
	tests := []struct {
		name        string
		contentHash string
		generation  int
		segments    []Segment
		want        string
	}{
		{name: "missing identity", generation: 1, segments: []Segment{valid}, want: "内容哈希"},
		{name: "missing segments", contentHash: "content", generation: 1, want: "没有镜头范围"},
		{name: "invalid range", contentHash: "content", generation: 1,
			segments: []Segment{mutateBaseIndexSegment(valid, func(value *Segment) { value.SourceEndFrame = 0 })}, want: "源帧范围"},
		{name: "placeholder", contentHash: "content", generation: 1,
			segments: []Segment{mutateBaseIndexSegment(valid, func(value *Segment) { value.Description = "待理解视频片段 0" })}, want: "placeholder"},
		{name: "missing frame", contentHash: "content", generation: 1,
			segments: []Segment{mutateBaseIndexSegment(valid, func(value *Segment) { value.RepresentativeFrames = nil })}, want: "缺少代表帧"},
		{name: "invalid frame", contentHash: "content", generation: 1,
			segments: []Segment{mutateBaseIndexSegment(valid, func(value *Segment) { value.RepresentativeFrames[0].ObjectHash = "short" })}, want: "manifest 无效"},
		{name: "missing quality", contentHash: "content", generation: 1,
			segments: []Segment{mutateBaseIndexSegment(valid, func(value *Segment) { value.Quality = "" })}, want: "质量事实"},
		{name: "missing labels", contentHash: "content", generation: 1,
			segments: []Segment{mutateBaseIndexSegment(valid, func(value *Segment) {
				value.Subjects, value.Actions, value.Setting, value.Tags = nil, nil, nil, nil
			})}, want: "结构化标签"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := BuildBaseIndexShots(test.contentHash, test.generation, Summary{Segments: test.segments}, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v want substring %q", err, test.want)
			}
		})
	}
}

func TestBuildBaseIndexShotsPreservesIdentityAcrossBoundaryRevision(t *testing.T) {
	segment := baseIndexTestSegment()
	segment.SourceStartFrame = 5
	segment.SourceEndFrame = 105
	segment.BoundaryVerified = true
	previous := []storage.IndexedShot{{
		ShotID: "shot_existing", SourceStartFrame: 0, SourceEndFrame: 100, BoundaryVersion: 3,
	}}
	shots, err := BuildBaseIndexShots("content", 2, Summary{Segments: []Segment{segment}}, previous)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 1 || shots[0].ShotID != "shot_existing" || shots[0].BoundaryVersion != 4 ||
		shots[0].LineageParentShotID == nil || *shots[0].LineageParentShotID != "shot_existing" ||
		shots[0].BoundaryConfidence == nil || *shots[0].BoundaryConfidence != 1 {
		t.Fatalf("shots=%#v", shots)
	}
	if shots[0].BoundaryKind != "analysis_window" || len(shots[0].SearchTokens) == 0 ||
		!strings.Contains(shots[0].SearchText, "person") ||
		shots[0].SemanticName != "office·person·walk" {
		t.Fatalf("search projection=%#v", shots[0])
	}
}

func TestBuildBaseIndexShotsKeepsCompactProviderSemanticName(t *testing.T) {
	segment := baseIndexTestSegment()
	segment.SemanticName = "  海边日落人物站立的超长语义名称应该被截断  "
	shots, err := BuildBaseIndexShots("content", 1, Summary{Segments: []Segment{segment}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 1 || shots[0].SemanticName != "海边日落人物站立的超长语义名称应该被" ||
		!strings.Contains(shots[0].SearchText, shots[0].SemanticName) {
		t.Fatalf("semantic projection=%#v", shots)
	}
}

func TestWithSemanticNamesUpgradesLegacySummary(t *testing.T) {
	legacy := Summary{Segments: []Segment{baseIndexTestSegment()}}
	upgraded := WithSemanticNames(legacy)
	if upgraded.Segments[0].SemanticName != "office·person·walk" ||
		legacy.Segments[0].SemanticName != "" {
		t.Fatalf("legacy=%#v upgraded=%#v", legacy.Segments[0], upgraded.Segments[0])
	}
}

func TestBuildBaseIndexShotsSortsAndRecordsPartialLineage(t *testing.T) {
	boundaryScore := 10.0
	overexposed := 0.02
	sharpness := 83.0
	partial := baseIndexTestSegment()
	partial.SourceStartFrame = 90
	partial.SourceEndFrame = 190
	partial.BoundaryScore = &boundaryScore
	partial.BoundaryVerified = true
	partial.OverexposedRatio = &overexposed
	partial.SharpnessScore = &sharpness
	shots, err := BuildBaseIndexShots("content", 2, Summary{Segments: []Segment{partial}}, []storage.IndexedShot{{
		ShotID: "shot_partial_parent", SourceStartFrame: 0, SourceEndFrame: 100, BoundaryVersion: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(shots) != 1 || shots[0].ShotID == "shot_partial_parent" ||
		shots[0].LineageParentShotID == nil || *shots[0].LineageParentShotID != "shot_partial_parent" ||
		shots[0].BoundaryConfidence == nil || *shots[0].BoundaryConfidence != 0.5 ||
		shots[0].Quality["overexposed_ratio"] != overexposed || shots[0].Quality["sharpness"] != sharpness {
		t.Fatalf("shots=%#v", shots)
	}

	first := baseIndexTestSegment()
	first.SourceStartFrame, first.SourceEndFrame = 10, 90
	second := baseIndexTestSegment()
	second.SourceStartFrame, second.SourceEndFrame = 0, 100
	third := baseIndexTestSegment()
	third.SourceStartFrame, third.SourceEndFrame = 0, 80
	sorted, err := BuildBaseIndexShots("content", 3, Summary{Segments: []Segment{first, second, third}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sorted) != 3 || sorted[0].SourceEndFrame != 80 || sorted[1].SourceEndFrame != 100 ||
		sorted[2].SourceStartFrame != 10 {
		t.Fatalf("sorted=%#v", sorted)
	}
	if rangeIntersectionOverUnion(0, 0, 0, 0) != 0 || len(compactStrings([]string{"", "  "})) != 0 {
		t.Fatal("degenerate range and blank labels must stay empty")
	}
	if matched, overlap := bestPreviousShot(partial, []storage.IndexedShot{{
		ShotID: "used", SourceStartFrame: 90, SourceEndFrame: 190,
	}}, map[string]struct{}{"used": {}}); matched != nil || overlap != 0 {
		t.Fatalf("used shot matched=%#v overlap=%f", matched, overlap)
	}
}

func baseIndexTestSegment() Segment {
	return Segment{
		SourceStartFrame: 0, SourceEndFrame: 100,
		Description: "A person walks through a bright office",
		Tags:        []string{"office", " office "}, Quality: "good",
		Subjects: []string{"person"}, Actions: []string{"walking"}, Setting: []string{"office"},
		ShotScale: "medium", Composition: "centered",
		RepresentativeFrames: []RepresentativeFrame{{
			SourceFrame: 50, TimestampMS: 2_000, Position: "middle",
			ObjectHash: strings.Repeat("a", 64), ObjectSize: 128,
		}},
	}
}

func mutateBaseIndexSegment(segment Segment, mutate func(*Segment)) Segment {
	segment.RepresentativeFrames = append([]RepresentativeFrame(nil), segment.RepresentativeFrames...)
	mutate(&segment)
	return segment
}
