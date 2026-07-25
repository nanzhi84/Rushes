package agentexec

import "strings"

type TalkingHeadRange struct {
	Start int
	End   int
}

func TalkingHeadTranscriptText(
	utterances []SpeechUtterance,
	startFrame, endFrame int,
) string {
	parts := []string{}
	for _, utterance := range utterances {
		if utterance.EndFrame <= startFrame || utterance.StartFrame >= endFrame {
			continue
		}
		if len(utterance.Words) == 0 {
			parts = append(parts, utterance.Text)
			continue
		}
		text := ""
		for _, word := range utterance.Words {
			if word.EndFrame <= startFrame || word.StartFrame >= endFrame {
				continue
			}
			text += word.Text + word.Punctuation
		}
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "")
}
