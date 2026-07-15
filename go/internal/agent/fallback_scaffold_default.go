//go:build !e2e_scaffold

package agent

func newFallbackScaffold(*Service) fallbackScaffold {
	return nil
}
