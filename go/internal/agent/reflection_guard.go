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
const reflectionRestatePrompt = `你是回复整形器。下面是一段面向用户的最终回复,但它夹带了本应内部消化的过程性思考(自我怀疑、中途推翻、二次确认之类)。请把它重写成一段干净、笃定的最终回复:只保留结论与已完成的事实,去掉「但等等」「让我再确认」「不对,重新想」这类过程性表达,不新增任何信息、不提问,直接给结论。只输出重写后的回复正文。`

// reflectionLeakMarkers 是「过程性思考漏进最终回复」的高信号标记:自我怀疑、中途推翻、
// 二次确认一类。刻意收紧到高信号搭配——裸「等等，」「不对，」会误伤合法中文(如「等等下一个
// 镜头」「不对，是第二段」),故只留「但等等」这类明确转折,以及「不对，重新」这类明确推翻。
// 全部小写(中文不受 ToLower 影响),检测时对回复统一小写后子串匹配。
var reflectionLeakMarkers = []string{
	"但等等", "让我再确认", "让我再检查", "让我重新", "我需要重新",
	"重新想一下", "重新考虑一下", "不对，重新", "不对,重新", "先别急,",
	"let me reconsider", "let me double-check", "hold on", "wait, ", "actually, wait",
}

// matchedReflectionMarker 返回终态回复命中的第一个反思标记,未命中返回空串。纯字符串匹配、
// 零模型调用,正常回复零额外延迟。
func matchedReflectionMarker(content string) string {
	lowered := strings.ToLower(content)
	for _, marker := range reflectionLeakMarkers {
		if strings.Contains(lowered, marker) {
			return marker
		}
	}
	return ""
}

func finalReplyHasReflectionLeak(content string) bool {
	return matchedReflectionMarker(content) != ""
}

// qualityCheckedFinalReply 命中反思泄漏时要求模型把回复重述干净一次(最多 1 次)。重述成功
// 且不再夹带过程性语句才采用并返回 restated=true;否则原样放行、记日志。未命中或无模型时零
// 额外开销。
//
// 时序契约(与 H5 直通流式):重述发生在流式回合完成之后,只改「事后」的规范回复,不拦流式。
// 因此 turn-stream 上会先流出**原文**的 text_delta(实时可能闪现夹带反思的原句),回合收尾时
// message_completed 携带的是**重述版**整体替换,持久化消息也是重述版;这份「实时闪现、事后
// 干净」是有意为之——不为 P2 的观感牺牲 H5 的 TTFT(首字延迟),而泄漏窗口在直通下极短。
func (service *Service) qualityCheckedFinalReply(
	ctx context.Context, draftID, messageID, content string,
) (string, bool) {
	marker := matchedReflectionMarker(content)
	if marker == "" || service.chatModel == nil {
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
			"draft_id", draftID, "message_id", messageID, "marker", marker, "reason", reason)
		return content, false
	}
	// 悬空度量:H3 落地前先有结构化日志可查重述发生率(哪条草稿/消息、命中哪个标记)。
	slog.Info("终态回复反思泄漏,已重述并采用",
		"draft_id", draftID, "message_id", messageID, "marker", marker)
	return restated, true
}
