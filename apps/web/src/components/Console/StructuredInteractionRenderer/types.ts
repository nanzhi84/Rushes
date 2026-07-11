import type { Decision, DecisionAnswer } from "../../../api/client";

export type StructuredInteractionItem =
  | DecisionInteractionItem
  | ProgressInteractionItem
  | ErrorInteractionItem
  | PreviewInteractionItem
  | TimelineInteractionItem
  | UnknownInteractionItem;

export type DecisionInteractionItem = {
  kind: "decision";
  id: string;
  decision_id: string;
  decision: Decision | null;
  status: Decision["status"];
  answer?: DecisionAnswer | null;
  createdAt?: string | null;
};

export type ProgressInteractionItem = {
  kind: "progress";
  id: string;
  job_id: string;
  job_kind: string;
  progress: number | null;
  status: "queued" | "running" | "succeeded" | "failed" | "cancelled";
};

export type ErrorInteractionItem = {
  kind: "error";
  id: string;
  error_code: string;
  message: string;
  retryable: boolean;
  details?: unknown;
};

export type PreviewInteractionItem = {
  kind: "preview";
  id: string;
  title: string;
  description: string;
  occurrences?: number;
};

export type TimelineInteractionItem = {
  kind: "timeline";
  id: string;
  title: string;
  description: string;
  occurrences?: number;
};

export type UnknownInteractionItem = {
  kind: "unknown";
  id: string;
  eventName: string;
  raw: unknown;
};

export type DomainSsePayload = {
  event_id: number;
  event: DomainSseEvent;
};

export type DomainSseEvent = {
  event: string;
  draft_id?: string | null;
  requested_by_draft_id?: string | null;
  decision_id?: string | null;
  job_id?: string | null;
  [key: string]: unknown;
};

export type AnswerDecisionHandler = (decisionId: string, answer: DecisionAnswer) => void;
