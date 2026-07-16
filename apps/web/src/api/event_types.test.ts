import eventNames from "../../../../go/internal/contracts/testdata/sse_event_names.golden.json";
import { KNOWN_TURN_STREAM_TYPES } from "../components/Console/useTurnStream";
import { ALL_EVENT_TYPES, DRAFT_EVENT_TYPES, WORKSPACE_EVENT_TYPES } from "./event_types";

describe("SSE 事件清单", () => {
  it("领域事件、路由与 Go 共享 golden 对拍", () => {
    expect(sorted(ALL_EVENT_TYPES)).toEqual(sorted(eventNames.domain_event_types));
    expect(sorted(DRAFT_EVENT_TYPES)).toEqual(sorted(eventNames.draft_event_types));
    expect(sorted(WORKSPACE_EVENT_TYPES)).toEqual(sorted(eventNames.workspace_event_types));
  });

  it("turn stream 运行时清单与 Go 共享 golden 对拍", () => {
    expect(sorted(KNOWN_TURN_STREAM_TYPES)).toEqual(sorted(eventNames.turn_stream_types));
  });
});

function sorted(values: readonly string[]): string[] {
  return [...values].sort();
}
