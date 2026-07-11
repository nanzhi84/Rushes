// Command specfix normalizes the frozen migration-baseline OpenAPI document.
// The original contract reused operation IDs for paired GET/HEAD routes;
// oapi-codegen correctly rejects that ambiguity.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

var httpMethods = map[string]struct{}{
	"get": {}, "head": {}, "post": {}, "put": {}, "patch": {}, "delete": {},
}

func main() {
	check := flag.Bool("check", false, "只校验，不写回")
	compatOutput := flag.String("compat-output", "", "写出供 oapi-codegen 使用的 OpenAPI 3.0.3 派生文件")
	flag.Parse()
	if flag.NArg() != 1 {
		fatal(errors.New("用法: specfix [-check] <openapi.json>"))
	}
	path := flag.Arg(0)
	data, err := os.ReadFile(path)
	if err != nil {
		fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		fatal(err)
	}
	operationIDsChanged, err := fixOperationIDs(document)
	if err != nil {
		fatal(err)
	}
	if *check {
		if *compatOutput != "" {
			fatal(errors.New("-check 与 -compat-output 不能同时使用"))
		}
		if operationIDsChanged {
			fatal(errors.New("OpenAPI 仍含重复 operationId"))
		}
		return
	}
	outputPath := path
	if *compatOutput != "" {
		downgradeOpenAPI31(document)
		outputPath = *compatOutput
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		fatal(err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(outputPath, encoded, 0o644); err != nil {
		fatal(err)
	}
}

func downgradeOpenAPI31(document map[string]any) bool {
	changed := false
	if version, _ := document["openapi"].(string); version != "3.0.3" {
		document["openapi"] = "3.0.3"
		changed = true
	}
	if _, exists := document["jsonSchemaDialect"]; exists {
		delete(document, "jsonSchemaDialect")
		changed = true
	}
	if normalizeSchemaNode(document) {
		changed = true
	}
	return changed
}

func normalizeSchemaNode(node any) bool {
	changed := false
	switch value := node.(type) {
	case []any:
		for _, item := range value {
			if normalizeSchemaNode(item) {
				changed = true
			}
		}
	case map[string]any:
		if constant, exists := value["const"]; exists {
			if _, hasEnum := value["enum"]; !hasEnum {
				value["enum"] = []any{constant}
			}
			delete(value, "const")
			changed = true
		}
		if rawAnyOf, exists := value["anyOf"]; exists {
			if alternatives, ok := rawAnyOf.([]any); ok {
				nonNull := make([]any, 0, len(alternatives))
				hadNull := false
				for _, alternative := range alternatives {
					if schema, ok := alternative.(map[string]any); ok && schema["type"] == "null" {
						hadNull = true
						continue
					}
					nonNull = append(nonNull, alternative)
				}
				if hadNull && len(nonNull) > 0 {
					value["nullable"] = true
					if len(nonNull) == 1 {
						if schema, ok := nonNull[0].(map[string]any); ok {
							delete(value, "anyOf")
							for key, item := range schema {
								if _, exists := value[key]; !exists {
									value[key] = item
								}
							}
						} else {
							value["anyOf"] = nonNull
						}
					} else {
						value["anyOf"] = nonNull
					}
					changed = true
				}
			}
		}
		for _, item := range value {
			if normalizeSchemaNode(item) {
				changed = true
			}
		}
	}
	return changed
}

func fixOperationIDs(document map[string]any) (bool, error) {
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		return false, errors.New("OpenAPI 缺少 paths object")
	}
	type operation struct {
		path   string
		method string
		value  map[string]any
		id     string
	}
	var operations []operation
	counts := map[string]int{}
	for path, rawPathItem := range paths {
		pathItem, ok := rawPathItem.(map[string]any)
		if !ok {
			continue
		}
		for method, rawOperation := range pathItem {
			if _, ok := httpMethods[method]; !ok {
				continue
			}
			value, ok := rawOperation.(map[string]any)
			if !ok {
				continue
			}
			id, _ := value["operationId"].(string)
			if id == "" {
				return false, fmt.Errorf("%s %s 缺少 operationId", method, path)
			}
			operations = append(operations, operation{path: path, method: method, value: value, id: id})
			counts[id]++
		}
	}
	sort.Slice(operations, func(i, j int) bool {
		if operations[i].path == operations[j].path {
			return operations[i].method < operations[j].method
		}
		return operations[i].path < operations[j].path
	})
	changed := false
	for _, operation := range operations {
		if counts[operation.id] < 2 {
			continue
		}
		base := operation.id
		for method := range httpMethods {
			base = strings.TrimSuffix(base, "_"+method)
		}
		operation.value["operationId"] = base + "_" + operation.method
		changed = true
	}
	return changed, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
