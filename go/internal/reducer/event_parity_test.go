package reducer

import (
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
)

func TestEventApplyRegistryMatchesContractRegistry(t *testing.T) {
	t.Parallel()

	for eventType := range contracts.EventRegistry {
		if eventApplyRegistry[eventType] == nil {
			t.Errorf("事件 %s 已登记契约但 reducer 未登记 apply", eventType)
		}
	}
	for eventType := range eventApplyRegistry {
		if _, ok := contracts.EventRegistry[eventType]; !ok {
			t.Errorf("事件 %s 已登记 reducer apply 但不在事件契约", eventType)
		}
	}
}
