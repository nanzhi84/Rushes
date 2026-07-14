import { useMemo, useState } from "react";
import type { FormEvent, ReactElement } from "react";
import {
  Check,
  ChevronRight,
  CircleAlert,
  CircleHelp,
  LoaderCircle
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
import { formatElapsedTime, useElapsedSeconds } from "../useElapsedTime";

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
        <DecisionInteractionGroup
          items={[item]}
          onAnswerDecision={onAnswerDecision}
          answerPending={answerPending}
        />
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

export function DecisionInteractionGroup({
  items,
  onAnswerDecision,
  answerPending = false
}: {
  items: DecisionInteractionItem[];
  onAnswerDecision: AnswerDecisionHandler;
  answerPending?: boolean;
}): ReactElement {
  const pendingCount = items.filter((item) => item.status === "pending").length;
  const answeredCount = items.filter((item) => item.status === "answered").length;
  const summary =
    pendingCount > 0
      ? `${pendingCount} 个问题待回答`
      : answeredCount > 0
        ? `已回答 ${answeredCount} 个问题`
        : "确认已结束";

  return (
    <section
      className={`overflow-hidden rounded-sm border bg-raised ${
        pendingCount > 0 ? "border-accent/35" : "border-line"
      }`}
      data-testid="decision-group"
      aria-label={summary}
    >
      <div className="flex min-h-8 items-center gap-2 border-b border-line px-3 py-1.5 text-xs">
        {pendingCount > 0 ? (
          <CircleHelp
            size={13}
            strokeWidth={1.8}
            aria-hidden
            className="shrink-0 text-accent-strong"
          />
        ) : (
          <Check
            size={13}
            strokeWidth={2}
            aria-hidden
            className="shrink-0 text-accent-strong"
          />
        )}
        <span className="font-medium text-fg-muted">{summary}</span>
      </div>
      <div className="divide-y divide-line">
        {items.map((item, index) => (
          <DecisionQuestion
            key={item.id}
            item={item}
            index={index + 1}
            onAnswerDecision={onAnswerDecision}
            answerPending={answerPending}
          />
        ))}
      </div>
    </section>
  );
}

function DecisionQuestion({
  item,
  index,
  onAnswerDecision,
  answerPending
}: {
  item: DecisionInteractionItem;
  index: number;
  onAnswerDecision: AnswerDecisionHandler;
  answerPending: boolean;
}): ReactElement {
  const [freeText, setFreeText] = useState("");
  const decision = item.decision;
  const isPending = item.status === "pending";
  const isLoadingDecision = isPending && !decision;
  const placeholderTitle =
    item.status === "pending"
      ? "正在同步确认项"
      : item.status === "answered"
        ? "回答已记录"
        : "确认项已取消";
  const loadingSeconds = useElapsedSeconds(isLoadingDecision);
  const disabled = !isPending || answerPending || !decision;
  const answer = item.answer ?? decision?.answer ?? null;
  const answerLabel = useMemo(
    () => decisionAnswerLabel(decision, answer),
    [answer, decision]
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
    <article
      className="px-3 py-2.5"
      data-testid="decision-question"
      data-decision-id={item.decision_id}
    >
      <div className="flex min-w-0 items-start gap-2">
        <span
          className="mt-0.5 w-10 shrink-0 text-2xs font-medium leading-5 tabular-nums text-fg-faint"
        >
          问题 {index}
        </span>
        <div className="min-w-0 flex-1">
          <h3
            className={`text-[13px] font-medium leading-5 ${
              isPending ? "text-fg" : "text-fg-muted"
            }`}
          >
            {decision?.question ?? placeholderTitle}
          </h3>

          {decision && isPending ? (
            <div className="mt-2 space-y-2.5">
              {decision.options && decision.options.length > 0 ? (
                <div
                  className="divide-y divide-line overflow-hidden rounded-sm border border-line-strong bg-panel"
                  role="group"
                  aria-label="确认选项"
                >
                  {decision.options.map((option) => {
                    const selected = isSelectedOption(decision, answer, option);
                    return (
                      <button
                        key={option.option_id}
                        className={`group flex w-full items-start gap-2 px-2.5 py-1.5 text-left text-[13px] transition-colors duration-fast disabled:cursor-default ${
                          selected
                            ? "bg-selected text-fg"
                            : "text-fg hover:bg-hover disabled:bg-raised disabled:text-fg-faint"
                        }`}
                        type="button"
                        disabled={disabled}
                        onClick={() => onAnswerDecision(decision.decision_id, buttonAnswer(option))}
                      >
                        <span className="min-w-0 flex-1">
                          <span className="font-medium leading-5">{option.label}</span>
                          {option.description ? (
                            <span className="block text-xs leading-4 text-fg-muted">
                              {option.description}
                            </span>
                          ) : null}
                        </span>
                        {selected ? (
                          <Check
                            size={13}
                            strokeWidth={2}
                            aria-hidden
                            className="mt-1 shrink-0 text-accent-strong"
                          />
                        ) : (
                          <ChevronRight
                            size={13}
                            strokeWidth={1.8}
                            aria-hidden
                            className="mt-1 shrink-0 text-fg-faint transition-transform duration-fast group-hover:translate-x-0.5"
                          />
                        )}
                      </button>
                    );
                  })}
                </div>
              ) : null}

              {decision.allow_free_text ? (
                <form className="flex gap-1.5" onSubmit={submitFreeText}>
                  <label className="min-w-0 flex-1">
                    <span className="sr-only">自由回答</span>
                    <input
                      aria-label="自由回答"
                      className="h-8 w-full rounded-sm border border-line-strong bg-panel px-2.5 text-[13px] text-fg outline-none placeholder:text-fg-faint focus:border-accent disabled:bg-raised"
                      value={freeText}
                      onChange={(event) => setFreeText(event.target.value)}
                      disabled={disabled}
                      placeholder="其他回答"
                    />
                  </label>
                  <button
                    className="h-8 shrink-0 rounded-sm bg-accent px-2.5 text-xs font-semibold text-white transition-[transform,background-color] duration-fast hover:bg-accent-strong active:translate-y-px disabled:opacity-40"
                    type="submit"
                    disabled={disabled || freeText.trim().length === 0}
                  >
                    提交
                  </button>
                </form>
              ) : null}
            </div>
          ) : isPending ? (
            <div className="mt-1.5 flex items-center gap-2 text-xs text-fg-muted">
              <LoaderCircle
                size={13}
                strokeWidth={1.8}
                aria-hidden
                className="shrink-0 animate-spin text-accent"
              />
              <span className="min-w-0 flex-1">正在读取可选项</span>
              <time
                className="shrink-0 font-mono text-2xs tabular-nums text-fg-faint"
                aria-label={`已用时 ${loadingSeconds} 秒`}
              >
                已用 {formatElapsedTime(loadingSeconds)}
              </time>
            </div>
          ) : item.status === "answered" ? (
            <div
              className="mt-2 border-l-2 border-accent bg-selected px-2.5 py-1.5"
              data-testid="decision-answer"
            >
              <span className="block text-2xs font-medium leading-4 text-accent-strong">
                回答
              </span>
              <p className="mt-0.5 min-w-0 break-words whitespace-pre-wrap text-[13px] leading-5 text-fg">
                {answerLabel ?? "回答已记录"}
              </p>
            </div>
          ) : (
            <p className="mt-0.5 text-xs leading-5 text-fg-faint">已取消</p>
          )}
        </div>
      </div>
    </article>
  );
}

function ProgressRow({ item }: { item: ProgressInteractionItem }): ReactElement {
  const view = progressRowView(item);
  const elapsedSeconds = useElapsedSeconds(view.active);
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
        {view.active ? (
          <time
            className="shrink-0 font-mono text-2xs tabular-nums text-fg-faint"
            aria-label={`已用时 ${elapsedSeconds} 秒`}
          >
            已用 {formatElapsedTime(elapsedSeconds)}
          </time>
        ) : null}
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

function decisionAnswerLabel(decision: Decision | null, answer: DecisionAnswer | null): string | null {
  if (!answer) {
    return null;
  }
  const option = decision?.options?.find((item) => item.option_id === answer.option_id);
  if (option && answer.free_text) {
    return `${option.label}：${answer.free_text}`;
  }
  if (option) {
    return option.label;
  }
  if (answer.free_text) {
    return answer.free_text;
  }
  return answer.option_id ?? "已回答";
}

function isSelectedOption(
  decision: Decision,
  answer: DecisionAnswer | null,
  option: DecisionOption
): boolean {
  return Boolean(answer?.option_id && answer.option_id === option.option_id && decision.status !== "pending");
}
