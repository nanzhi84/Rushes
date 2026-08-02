package main

import (
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/providers"
)

func TestDashScopeVisionModelDefaultsToVisionTier(t *testing.T) {
	t.Setenv("RUSHES_QWEN_VISION_MODEL", "")
	if got := dashScopeVisionModelName(); got != providers.DefaultVisionModel {
		t.Fatalf("model=%q want=%q", got, providers.DefaultVisionModel)
	}
	t.Setenv("RUSHES_QWEN_VISION_MODEL", "custom-vision")
	if got := dashScopeVisionModelName(); got != "custom-vision" {
		t.Fatalf("model=%q want custom override", got)
	}
}
