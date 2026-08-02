//go:build e2e_scaffold

package main

import (
	"context"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

// e2eVisionModel keeps browser E2E deterministic and offline while exercising
// the production frame extraction, search_ready validation and persistence path.
type e2eVisionModel struct{}

func (e2eVisionModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return e2eVisionModel{}, nil
}

func (e2eVisionModel) Generate(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.Message, error) {
	return schema.AssistantMessage(`{"overall":"自动索引测试视频","semantic_role":"b_roll","segments":[{"id":"s000","description":"纯色测试画面，全景居中构图","tags":["测试画面"],"quality":"usable","subjects":["测试画面"],"actions":[],"setting":["纯色背景"],"shot_scale":"全景","composition":"居中构图","lighting":["均匀"],"mood":["中性"],"edit_hints":["测试"]}]}`, nil), nil
}

func (value e2eVisionModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := value.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func defaultAnalyzer() *understanding.Analyzer {
	return understanding.NewAnalyzer(e2eVisionModel{})
}
