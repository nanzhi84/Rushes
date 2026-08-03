package tools

import (
	"errors"
	"fmt"
	"strings"
)

type ToolLoadInput struct {
	ToolNames []string `json:"tool_names"`
}

type ToolLoadResult struct {
	Status        string   `json:"status"`
	LoadedNames   []string `json:"loaded_names"`
	AlreadyLoaded []string `json:"already_loaded"`
	NotLoadable   []string `json:"not_loadable"`
}

func ValidateToolLoadInput(input ToolLoadInput) error {
	if len(input.ToolNames) < 1 || len(input.ToolNames) > 5 {
		return errors.New("tool.load 的 tool_names 必须包含 1 到 5 个名称")
	}
	seen := make(map[string]struct{}, len(input.ToolNames))
	for _, rawName := range input.ToolNames {
		name := strings.TrimSpace(rawName)
		if name == "" {
			return errors.New("tool.load 不接受空工具名")
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("tool.load 不接受重复工具名: %s", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}
