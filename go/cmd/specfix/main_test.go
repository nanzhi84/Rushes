package main

import (
	"reflect"
	"testing"
)

func TestFixOperationIDs(t *testing.T) {
	document := map[string]any{
		"paths": map[string]any{
			"/media": map[string]any{
				"get":  map[string]any{"operationId": "media_head"},
				"head": map[string]any{"operationId": "media_head"},
			},
		},
	}
	changed, err := fixOperationIDs(document)
	if err != nil || !changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
	paths := document["paths"].(map[string]any)["/media"].(map[string]any)
	if got := paths["get"].(map[string]any)["operationId"]; got != "media_get" {
		t.Fatalf("GET operationId=%v", got)
	}
	if got := paths["head"].(map[string]any)["operationId"]; got != "media_head" {
		t.Fatalf("HEAD operationId=%v", got)
	}
	changed, err = fixOperationIDs(document)
	if err != nil || changed {
		t.Fatalf("第二次应幂等: changed=%t err=%v", changed, err)
	}
}

func TestDowngradeOpenAPI31(t *testing.T) {
	document := map[string]any{
		"openapi":           "3.1.0",
		"jsonSchemaDialect": "https://json-schema.org/draft/2020-12/schema",
		"components": map[string]any{
			"schemas": map[string]any{
				"MaybeString": map[string]any{
					"title": "MaybeString",
					"anyOf": []any{
						map[string]any{"type": "string"},
						map[string]any{"type": "null"},
					},
				},
				"Literal": map[string]any{"type": "string", "const": "ok"},
			},
		},
	}
	if !downgradeOpenAPI31(document) {
		t.Fatal("第一次转换应有变化")
	}
	if document["openapi"] != "3.0.3" {
		t.Fatalf("openapi=%v", document["openapi"])
	}
	if _, exists := document["jsonSchemaDialect"]; exists {
		t.Fatal("应移除 jsonSchemaDialect")
	}
	schemas := document["components"].(map[string]any)["schemas"].(map[string]any)
	maybe := schemas["MaybeString"].(map[string]any)
	if maybe["type"] != "string" || maybe["nullable"] != true {
		t.Fatalf("nullable schema=%#v", maybe)
	}
	literal := schemas["Literal"].(map[string]any)
	if _, exists := literal["const"]; exists {
		t.Fatal("const 应被转换")
	}
	if !reflect.DeepEqual(literal["enum"], []any{"ok"}) {
		t.Fatalf("enum=%#v", literal["enum"])
	}
	if downgradeOpenAPI31(document) {
		t.Fatal("第二次转换应幂等")
	}
}
