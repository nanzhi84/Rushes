package agentexec

import (
	"strings"
	"testing"

	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

func TestDeepVerificationAndCandidateSortAreDeterministic(t *testing.T) {
	criterion := func(status string) rushestools.ShotDeepCriterionEvidence {
		return rushestools.ShotDeepCriterionEvidence{Criterion: status, Status: status}
	}
	tests := []struct {
		name                                  string
		requirements, exclusions, preferences []rushestools.ShotDeepCriterionEvidence
		verification                          string
	}{
		{name: "refuted requirement", requirements: []rushestools.ShotDeepCriterionEvidence{criterion("refuted")}, verification: "reject"},
		{name: "observed exclusion", requirements: []rushestools.ShotDeepCriterionEvidence{criterion("observed")}, exclusions: []rushestools.ShotDeepCriterionEvidence{criterion("observed")}, verification: "reject"},
		{name: "all requirements", requirements: []rushestools.ShotDeepCriterionEvidence{criterion("observed")}, preferences: []rushestools.ShotDeepCriterionEvidence{criterion("observed")}, verification: "match"},
		{name: "no requirements", preferences: []rushestools.ShotDeepCriterionEvidence{criterion("observed")}, verification: "match"},
		{name: "partial", requirements: []rushestools.ShotDeepCriterionEvidence{criterion("observed"), criterion("uncertain")}, verification: "partial"},
		{name: "uncertain", requirements: []rushestools.ShotDeepCriterionEvidence{criterion("uncertain")}, verification: "uncertain"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			verification, score := applyDeepVerification(test.requirements, test.exclusions, test.preferences)
			if verification != test.verification || score < 0 || score > 1 {
				t.Fatalf("verification=%q score=%v", verification, score)
			}
		})
	}

	values := []rushestools.ShotDeepCandidate{
		{AssetID: "z", ShotID: "shot_b", Verification: "reject", Score: 1},
		{AssetID: "z", ShotID: "shot_b", Verification: "match", Score: 0.8},
		{AssetID: "b", ShotID: "shot_a", Verification: "match", Score: 0.8},
		{AssetID: "a", ShotID: "shot_a", Verification: "match", Score: 0.8},
		{AssetID: "z", ShotID: "shot_c", Verification: "match", Score: 0.9},
		{AssetID: "z", ShotID: "shot_d", Verification: "partial", Score: 1},
		{AssetID: "z", ShotID: "shot_e", Verification: "uncertain", Score: 1},
	}
	sortDeepCandidates(values)
	want := []string{"z:shot_c", "a:shot_a", "b:shot_a", "z:shot_b", "z:shot_d", "z:shot_e", "z:shot_b"}
	for index, candidate := range values {
		if got := candidate.AssetID + ":" + candidate.ShotID; got != want[index] {
			t.Fatalf("sorted[%d]=%s want=%s values=%#v", index, got, want[index], values)
		}
	}
}

func TestDeepCriteriaCacheAndStoredFactGuards(t *testing.T) {
	if got, err := normalizeDeepCriteriaInput([]string{" one ", "two"}); err != nil ||
		len(got) != 2 || got[0] != "one" {
		t.Fatalf("normalized=%#v err=%v", got, err)
	}
	for name, values := range map[string][]string{
		"too many":  make([]string, 13),
		"blank":     {" "},
		"too long":  {strings.Repeat("长", 241)},
		"duplicate": {"same", " same "},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeDeepCriteriaInput(values); err == nil {
				t.Fatalf("invalid criteria accepted: %#v", values)
			}
		})
	}

	var nilCache *shotDeepQueryCache
	if _, exists := nilCache.get("missing"); exists {
		t.Fatal("nil cache should miss")
	}
	nilCache.put("ignored", rushestools.ShotDeepSearchResult{})
	cache := newShotDeepQueryCache()
	for index := 0; index <= maximumDeepQueryCacheEntries; index++ {
		key := string(rune('a' + index))
		cache.put(key, rushestools.ShotDeepSearchResult{Query: key})
	}
	if _, exists := cache.get("a"); exists || len(cache.values) != maximumDeepQueryCacheEntries {
		t.Fatalf("bounded cache size=%d oldest_exists=%v", len(cache.values), exists)
	}
	latestKey := string(rune('a' + maximumDeepQueryCacheEntries))
	if latest, exists := cache.get(latestKey); !exists || latest.Query != latestKey {
		t.Fatalf("latest=%#v exists=%v", latest, exists)
	}
	cache.put(latestKey, rushestools.ShotDeepSearchResult{Query: "updated"})
	if len(cache.order) != maximumDeepQueryCacheEntries {
		t.Fatalf("覆盖 cache entry 不应扩张 order: %d", len(cache.order))
	}

	frame := understanding.DeepFrameManifest{
		FrameID: "f1", SourceFrame: 10, ObjectHash: strings.Repeat("a", 64), ObjectSize: 1,
	}
	valid := storedDeepShotFacts{
		ShotID: "shot", SourceStartFrame: 0, SourceEndFrame: 30, BoundaryVersion: 1,
		Facets: []string{"appearance"}, Frames: []understanding.DeepFrameManifest{frame},
		Observations: []understanding.DeepObservation{{
			Facet: "appearance", Statement: "人物可见", FrameIDs: []string{"f1"},
		}},
	}
	if err := validateStoredDeepFacts(valid); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*storedDeepShotFacts){
		"missing core":        func(value *storedDeepShotFacts) { value.ShotID = "" },
		"blank facet":         func(value *storedDeepShotFacts) { value.Facets = []string{""} },
		"invalid frame":       func(value *storedDeepShotFacts) { value.Frames[0].ObjectHash = "bad" },
		"duplicate frame":     func(value *storedDeepShotFacts) { value.Frames = append(value.Frames, value.Frames[0]) },
		"invalid observation": func(value *storedDeepShotFacts) { value.Observations[0].Statement = "" },
		"undeclared facet":    func(value *storedDeepShotFacts) { value.Observations[0].Facet = "text_ocr" },
		"unknown frame":       func(value *storedDeepShotFacts) { value.Observations[0].FrameIDs = []string{"missing"} },
		"duplicate ref":       func(value *storedDeepShotFacts) { value.Observations[0].FrameIDs = []string{"f1", "f1"} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.Frames = append([]understanding.DeepFrameManifest(nil), valid.Frames...)
			candidate.Observations = append([]understanding.DeepObservation(nil), valid.Observations...)
			candidate.Observations[0].FrameIDs = append([]string(nil), valid.Observations[0].FrameIDs...)
			mutate(&candidate)
			if err := validateStoredDeepFacts(candidate); err == nil {
				t.Fatalf("invalid stored fact accepted: %#v", candidate)
			}
		})
	}
}
