package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func fmtJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func timelineTrackClips(document timeline.Document, trackID string) []timeline.Clip {
	for _, track := range document.Tracks {
		if track.TrackID == trackID {
			return track.Clips
		}
	}
	return nil
}

func seedTimelineVersion(
	service *Service,
	ctx context.Context,
	draftID string,
	document timeline.Document,
	operation string,
	editOperation map[string]any,
) (rushestools.ToolResult, error) {
	draft, err := storage.GetDraft(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	documentMap, err := timeline.ToMap(document)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	report := timeline.Validate(document)
	reportMap := map[string]any{
		"valid": report.Valid, "checks": report.Checks, "issues": report.Issues,
	}
	validationType := "TimelineValidated"
	if !report.Valid {
		validationType = "TimelineValidationFailed"
	}
	actor := contracts.ActorAgent
	origin := rushestools.TimelineMutationOrigin(ctx)
	if origin == "manual" {
		actor = contracts.ActorUser
	}
	if origin == "" {
		origin = "agent"
	}
	editOperations := []map[string]any{}
	if editOperation != nil {
		editOperations = append(editOperations, editOperation)
	}
	payload := map[string]any{
		"timeline_id": document.TimelineID, "timeline_version": document.Version,
		"patch_id":      operation + ":" + agentexec.RandomID("patch"),
		"document_json": documentMap, "edit_origin": origin,
		"edit_operations": editOperations,
	}
	if document.Version > 1 {
		payload["parent_version"] = document.Version - 1
	}
	result, err := reducer.Apply(ctx, service.database, []contracts.Event{
		{
			Type: "TimelineVersionCreated", DraftID: draftID,
			Payload: payload,
		},
		{
			Type: validationType, DraftID: draftID,
			Payload: map[string]any{
				"timeline_version":  document.Version,
				"validation_report": reportMap,
			},
		},
	}, reducer.Options{Actor: actor, BaseVersion: &draft.StateVersion})
	if err != nil || result.Status != reducer.StatusApplied {
		return rushestools.ToolResult{}, errors.Join(
			err, fmt.Errorf("timeline fixture reducer status: %s", result.Status),
		)
	}
	status := string(rushestools.StatusSucceeded)
	if !report.Valid {
		status = string(rushestools.StatusValidationFailed)
	}
	return rushestools.ToolResult{
		Status: status, Observation: timeline.Inspect(document),
		Data: map[string]any{"validation_report": reportMap},
	}, nil
}
