import { useMemo, useState } from "react";
import type { FormEvent, ReactElement } from "react";
import {
  Check,
  ChevronRight,
  CircleAlert,
  CircleHelp
} from "lucide-react";
import type { Decision, DecisionAnswer, DecisionOption } from "../../../api/client";
import type {
  AnswerDecisionHandler,
  DecisionInteractionItem,
  ErrorInteractionItem,
  PreviewInteractionItem,
  ProgressInteractionItem,
  StructuredInteractionItem,
  TimelineInteractionItem,
  UnknownInteractionItem
} from "./types";

export function StructuredInteractionRenderer({
  item,
  onAnswerDecision,
  answerPending = false
}: {
  item: StructuredInteractionItem;
  onAnswerDecision: AnswerDecisionHandler;
  answerPending?: boolean;
}): ReactElement {
  switch (item.kind) {
    case "decision":
      return (
        <DecisionCard item={item} onAnswerDecision={onAnswerDecision} answerPending={answerPending} />
      );
    case "progress":
      return <ProgressRow item={item} />;
    case "error":
      return <ErrorRow item={item} />;
    case "preview":
      return <EventRow item={item} />;
    case "timeline":
      return <EventRow item={item} />;
    case "unknown":
      return <UnknownRow item={item} />;
  }
}

function DecisionCard({
  item,
  onAnswerDecision,
  answerPending
}: {
  item: DecisionInteractionItem;
  onAnswerDecision: AnswerDecisionHandler;
  answerPending: boolean;
}): ReactElement {
  const [freeText, setFreeText] = useState("");
  const decision = item.decision;
  const isPending = item.status === "pending";
  const disabled = !isPending || answerPending || !decision;
  const answerLabel = useMemo(
    () => (decision ? decisionAnswerLabel(decision, item.answer ?? decision.answer ?? null) : null),
    [decision, item.answer]
  );

  const submitFreeText = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const value = freeText.trim();
    if (!decision || !value || disabled) {
      return;
    }
    onAnswerDecision(decision.decision_id, {
      free_text: value,
      answered_via: "natural_language",
      payload: {}
    });
    setFreeText("");
  };

  return (
    <article className={decisionCardClass(isPending)} data-testid="decision-card">
      <div className="flex items-start gap-2.5 border-b border-line px-3 py-2.5">
        <span className="grid size-7 shrink-0 place-items-center rounded-md bg-selected text-accent-strong">
          <CircleHelp size={15} strokeWidth={1.8} aria-hidden />
        </span>
        <div className="min-w-0 flex-1">
          <p className="text-2xs font-medium text-accent-strong">需要你的选择</p>
          <h3 className="mt-0.5 text-sm font-semibold leading-5 text-fg">
            {decision?.question ?? "确认项已创建，正在读取详情"}
          </h3>
        </div>
        <span className="shrink-0 pt-0.5 text-2xs font-medium text-fg-muted">
          {decisionStatusLabel(item.status)}
        </span>
      </div>

      {decision ? (
        <div className="space-y-3 px-3 py-3">
          {decision.options && decision.options.length > 0 ? (
            <div className="grid gap-1.5" role="group" aria-label="确认选项">
              {decision.options.map((option) => {
                const selected = isSelectedOption(decision, item.answer ?? decision.answer ?? null, option);
                return (
                <button
                  key={option.option_id}
                  className={`group flex items-start gap-2 rounded-md border px-2.5 py-2 text-left text-sm transition-[transform,background-color,border-color] duration-fast active:translate-y-px disabled:cursor-default ${
                    selected
                      ? "border-accent/50 bg-selected text-fg"
                      : "border-line-strong bg-panel text-fg hover:border-accent/40 hover:bg-hover disabled:border-line disabled:bg-raised disabled:text-fg-faint"
                  }`}
                  type="button"
                  disabled={disabled}
                  onClick={() => onAnswerDecision(decision.decision_id, buttonAnswer(option))}
                >
                  <span className="min-w-0 flex-1">
                    <span className="font-medium">{option.label}</span>
                    {option.description ? (
                      <span className="mt-0.5 block text-xs leading-5 text-fg-muted">{option.description}</span>
                    ) : null}
                  </span>
                  {selected ? (
                    <Check size={14} strokeWidth={2} aria-hidden className="mt-0.5 shrink-0 text-accent-strong" />
                  ) : (
                    <ChevronRight size={14} strokeWidth={1.8} aria-hidden className="mt-0.5 shrink-0 text-fg-faint transition-transform duration-fast group-hover:translate-x-0.5" />
                  )}
                </button>
                );
              })}
            </div>
          ) : null}

          {decision.allow_free_text ? (
            <form className="space-y-2 border-t border-line pt-3" onSubmit={submitFreeText}>
              <label className="block text-xs font-medium text-fg-muted">
                自由回答
                <input
                  className="mt-1.5 w-full rounded-md border border-line-strong bg-panel px-2.5 py-2 text-sm text-fg outline-none placeholder:text-fg-faint focus:border-accent disabled:bg-raised"
                  value={freeText}
                  onChange={(event) => setFreeText(event.target.value)}
                  disabled={disabled}
                  placeholder="也可以直接在消息输入里自然语言回答"
                />
              </label>
              <button
                className="rounded-md bg-accent px-3 py-1.5 text-xs font-semibold text-white transition-[transform,background-color] duration-fast hover:bg-accent-strong active:translate-y-px disabled:opacity-40"
                type="submit"
                disabled={disabled || freeText.trim().length === 0}
              >
                提交回答
              </button>
            </form>
          ) : null}

          <p className="text-2xs leading-4 text-fg-faint">可点选项，也可直接发消息回答。</p>
        </div>
      ) : (
        <p className="px-3 py-3 text-sm text-fg-muted">确认项详情会随当前状态查询补齐。</p>
      )}

      {!isPending && answerLabel ? (
        <p className="border-t border-line bg-selected px-3 py-2 text-sm text-fg-muted">
          结果：{answerLabel}
        </p>
      ) : null}
    </article>
  );
}

function ProgressRow({ item }: { item: ProgressInteractionItem }): ReactElement {
  const view = progressRowView(item);
  return (
    <div className="my-px py-0.5 text-xs" data-testid="progress-row" data-layout="inline">
      <div className="flex min-w-0 items-center gap-1.5">
        <span
          aria-hidden
          className={`size-1.5 shrink-0 rounded-full ${view.dotClass} ${view.active ? "animate-pulse" : ""}`}
        />
        <span className="min-w-0 flex-1 truncate text-fg-muted">{item.job_kind}</span>
        <span className={`shrink-0 text-2xs ${view.toneClass}`}>{view.statusText}</span>
        <span className="shrink-0 font-mono text-2xs tabular-nums text-fg-faint">
          {view.percent}%
        </span>
      </div>
      <div
        className="ml-3 mt-1 h-px overflow-hidden bg-line"
        role="progressbar"
        aria-label={`${item.job_kind} 进度`}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={view.percent}
      >
        <div className={`h-full ${view.barClass}`} style={{ width: `${view.percent}%` }} />
      </div>
    </div>
  );
}

type ProgressRowView = {
  statusText: string;
  percent: number;
  toneClass: string;
  barClass: string;
  dotClass: string;
  active: boolean;
};

// 终态（succeeded/failed/cancelled）必须让状态行显性收尾；即使没有中间进度，
// 也要靠 status 把「处理中」翻到最终状态。
function progressRowView(item: ProgressInteractionItem): ProgressRowView {
  switch (item.status) {
    case "succeeded":
      return {
        statusText: "已完成",
        percent: 100,
        toneClass: "text-ok",
        barClass: "bg-ok",
        dotClass: "bg-ok",
        active: false
      };
    case "failed":
      return {
        statusText: "失败",
        percent: item.progress ?? 0,
        toneClass: "text-danger",
        barClass: "bg-danger",
        dotClass: "bg-danger",
        active: false
      };
    case "cancelled":
      return {
        statusText: "已取消",
        percent: item.progress ?? 0,
        toneClass: "text-fg-muted",
        barClass: "bg-line-strong",
        dotClass: "bg-fg-faint",
        active: false
      };
    case "queued":
      return {
        statusText: "排队中",
        percent: item.progress ?? 0,
        toneClass: "text-info",
        barClass: "bg-accent",
        dotClass: "bg-fg-faint",
        active: true
      };
    default:
      return {
        statusText: "处理中",
        percent: item.progress ?? 0,
        toneClass: "text-info",
        barClass: "bg-accent",
        dotClass: "bg-fg-faint",
        active: true
      };
  }
}

function ErrorRow({ item }: { item: ErrorInteractionItem }): ReactElement {
  return (
    <div
      className="my-1 border-l-2 border-danger/60 py-0.5 pl-2.5 text-xs"
      data-testid="error-row"
      data-layout="inline"
    >
      <div className="flex min-w-0 items-start gap-1.5">
        <CircleAlert size={13} strokeWidth={1.8} aria-hidden className="mt-0.5 shrink-0 text-danger" />
        <p className="min-w-0 flex-1 leading-5 text-danger">
          <span className="font-medium">执行失败</span>
          <span className="ml-1 font-mono text-2xs text-fg-faint">{item.error_code}</span>
          <span className="block text-fg-muted">{item.message}</span>
        </p>
        <span className="shrink-0 text-2xs text-danger">
          {item.retryable ? "可重试" : "需调整"}
        </span>
      </div>
    </div>
  );
}

function EventRow({ item }: { item: PreviewInteractionItem | TimelineInteractionItem }): ReactElement {
  return (
    <div
      className="my-px flex min-w-0 items-start gap-1.5 py-0.5 text-xs text-fg-muted"
      data-testid={`${item.kind}-event-row`}
      data-layout="inline"
    >
      <span aria-hidden className="mt-1 size-1.5 shrink-0 rounded-full bg-ok" />
      <span className="shrink-0 font-medium text-fg-muted">{item.title}</span>
      <span className="min-w-0 flex-1 truncate text-fg-faint" title={item.description}>
        {item.description}
      </span>
      {item.occurrences && item.occurrences > 1 ? (
        <span className="shrink-0 font-mono text-2xs text-fg-faint">×{item.occurrences}</span>
      ) : null}
    </div>
  );
}

function UnknownRow({ item }: { item: UnknownInteractionItem }): ReactElement {
  return (
    <details className="group text-xs text-fg-muted" data-layout="inline">
      <summary className="-ml-1 inline-flex h-5 cursor-pointer list-none items-center gap-1.5 rounded-sm px-1 transition-colors duration-fast hover:bg-hover [&::-webkit-details-marker]:hidden">
        <span aria-hidden className="size-1.5 shrink-0 rounded-full bg-warn" />
        未知结构化事件：{item.eventName}
        <ChevronRight size={12} strokeWidth={1.8} aria-hidden className="transition-transform duration-base group-open:rotate-90" />
      </summary>
      <pre className="mt-1 max-h-40 overflow-auto border-l-2 border-line-strong/50 pl-3 font-mono text-2xs leading-4 text-fg-muted">
        {JSON.stringify(item.raw, null, 2)}
      </pre>
    </details>
  );
}

function buttonAnswer(option: DecisionOption): DecisionAnswer {
  return {
    option_id: option.option_id,
    answered_via: "button",
    payload: option.payload ?? {}
  };
}

function decisionAnswerLabel(decision: Decision, answer: DecisionAnswer | null): string | null {
  if (!answer) {
    return null;
  }
  const option = decision.options?.find((item) => item.option_id === answer.option_id);
  if (option) {
    return option.label;
  }
  if (answer.free_text) {
    return answer.free_text;
  }
  return answer.option_id ?? "已回答";
}

function decisionStatusLabel(status: Decision["status"]): string {
  if (status === "answered") {
    return "已回答";
  }
  if (status === "cancelled") {
    return "已取消";
  }
  return "待回答";
}

function decisionCardClass(isPending: boolean): string {
  const base = "overflow-hidden rounded-md border";
  return isPending
    ? `${base} border-accent/35 bg-raised`
    : `${base} border-line bg-raised opacity-80`;
}

function isSelectedOption(
  decision: Decision,
  answer: DecisionAnswer | null,
  option: DecisionOption
): boolean {
  return Boolean(answer?.option_id && answer.option_id === option.option_id && decision.status !== "pending");
}
