package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
)

func (service *Service) startAgentTurnRun(
	ctx context.Context,
	turnID string,
	item QueueItem,
) error {
	result, err := reducer.Apply(ctx, service.database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{AgentTurnRunStart: &reducer.AgentTurnRunStartRow{
			TurnID: turnID, DraftID: item.DraftID,
			SourceItemID: item.ItemID, Kind: string(item.Kind),
		}},
	})
	if err != nil {
		return err
	}
	if result.Status != reducer.StatusApplied {
		return fmt.Errorf("agent turn run start reducer status: %s", result.Status)
	}
	return nil
}

func (service *Service) finishAgentTurnRun(ctx context.Context, turnID, status string) error {
	finishContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	result, err := reducer.Apply(finishContext, service.database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{AgentTurnRunFinish: &reducer.AgentTurnRunFinishRow{
			TurnID: turnID, Status: status,
		}},
	})
	if err != nil {
		return err
	}
	if result.Status != reducer.StatusApplied {
		return fmt.Errorf("agent turn run finish reducer status: %s", result.Status)
	}
	return nil
}

func (service *Service) interruptStaleAgentTurnRuns(ctx context.Context) error {
	result, err := reducer.Apply(ctx, service.database, nil, reducer.Options{
		Actor:      contracts.ActorAgent,
		ResultRows: reducer.ResultRows{InterruptRunningAgentTurns: true},
	})
	if err != nil {
		return err
	}
	if result.Status != reducer.StatusApplied {
		return fmt.Errorf("interrupt agent turn runs reducer status: %s", result.Status)
	}
	return nil
}
