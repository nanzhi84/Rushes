package agent

import "context"

// fallbackScaffold contains deterministic fallback branches used only by the
// tagged E2E build. Production constructors receive nil from
// newFallbackScaffold, so fallbackTurn contains no test trigger strings.
type fallbackScaffold interface {
	TryHandle(
		ctx context.Context,
		draftID, messageID, content string,
	) (reply string, handled bool, err error)
}
