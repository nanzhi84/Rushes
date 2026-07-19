package agent

import (
	"context"
	"log/slog"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
)

// reflectionRestatePrompt 指导模型把夹带过程性思考的终态回复重写干净。只整形、不新增信息。
const reflectionRestatePrompt = `你是回复整形器。下面是一段面向用户的最终回复,但它夹带了本应内部消化的过程性思考(自我怀疑、中途推翻、二次确认之类)。请把它重写成一段干净、笃定的最终回复:只保留结论与已完成的事实,去掉「但等等」「让我再确认」「不对」这类过程性表达,不新增任何信息、不提问,直接给结论。只输出重写后的回复正文。`

// reflectionLeakMarkers 是「过程性思考漏进最终回复」的高信号标记:自我怀疑、中途推翻、
// 二次确认一类。全部小写(中文不受 ToLower 影响),检测时对回复统一小写后子串匹配。
var reflectionLeakMarkers = []string{
	"但等等", "等等，", "等一下,", "等一下，", "让我再确认", "让我再检查", "让我重新",
	"我需要重新", "重新想一下", "重新考虑一下", "不对，", "不对,", "先别急",
	"let me reconsider", "let me double-check", "hold on", "wait, ", "actually, wait",
}

// finalReplyHasReflectionLeak 轻量检测终态回复是否夹带过程性思考。纯字符串匹配、零模型
// 调用,正常回复零额外延迟。
func finalReplyHasReflectionLeak(content string) bool {
	lowered := strings.ToLower(content)
	for _, marker := range reflectionLeakMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// qualityCheckedFinalReply 命中反思泄漏时要求模型把回复重述干净一次(最多 1 次)。重述
// 成功且不再夹带过程性语句才采用并返回 restated=true;否则原样放行、记日志。未命中或无
// 模型时零额外开销。
func (service *Service) qualityCheckedFinalReply(
	ctx context.Context, draftID, messageID, content string,
) (string, bool) {
	if !finalReplyHasReflectionLeak(content) || service.chatModel == nil {
		return content, false
	}
	response, err := service.chatModel.Generate(ctx, []*schema.Message{
		schema.SystemMessage(reflectionRestatePrompt),
		schema.UserMessage(content),
	}, model.WithToolChoice(schema.ToolChoiceForbidden))
	restated := ""
	if response != nil {
		restated = strings.TrimSpace(response.Content)
	}
	if err != nil || restated == "" || finalReplyHasReflectionLeak(restated) {
		reason := "重述后仍夹带过程性语句"
		if err != nil {
			reason = agentexec.TruncateText(err.Error(), 300)
		} else if restated == "" {
			reason = "模型返回空重述"
		}
		slog.Warn("终态回复反思泄漏,重述未采用,原样放行",
			"draft_id", draftID, "message_id", messageID, "reason", reason)
		return content, false
	}
	return restated, true
}
