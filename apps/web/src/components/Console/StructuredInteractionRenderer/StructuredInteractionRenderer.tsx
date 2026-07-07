import { useMemo, useState } from "react";
import type { FormEvent, ReactElement } from "react";
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
      return <ProgressCard item={item} />;
    case "error":
      return <ErrorCard item={item} />;
    case "preview":
      return <PlaceholderCard item={item} label="预览" />;
    case "timeline":
      return <PlaceholderCard item={item} label="时间线" />;
    case "unknown":
      return <UnknownCard item={item} />;
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
      <div className="flex items-start justify-between gap-3">
        <div>
          <p className="text-xs font-medium text-fg-muted">确认项</p>
          <h3 className="mt-1 text-sm font-semibold text-fg">
            {decision?.question ?? "确认项已创建，正在读取详情"}
          </h3>
        </div>
        <span className="rounded border border-line-strong px-2 py-1 text-xs text-fg-muted">
          {decisionStatusLabel(item.status)}
        </span>
      </div>

      {decision ? (
        <div className="mt-4 space-y-3">
          {decision.options && decision.options.length > 0 ? (
            <div className="grid gap-2" role="group" aria-label="确认选项">
              {decision.options.map((option) => (
                <button
                  key={option.option_id}
                  className="rounded-md border border-line-strong bg-panel px-3 py-2 text-left text-sm text-fg hover:bg-hover disabled:bg-raised disabled:text-fg-faint"
                  type="button"
                  disabled={disabled}
                  onClick={() => onAnswerDecision(decision.decision_id, buttonAnswer(option))}
                >
                  <span className="font-medium">{option.label}</span>
                  {option.description ? (
                    <span className="mt-1 block text-xs leading-5 text-fg-muted">{option.description}</span>
                  ) : null}
                </button>
              ))}
            </div>
          ) : null}

          {decision.allow_free_text ? (
            <form className="space-y-2" onSubmit={submitFreeText}>
              <label className="block text-xs font-medium text-fg-muted">
                自由回答
                <input
                  className="mt-1 w-full rounded-md border border-line-strong px-3 py-2 text-sm outline-none focus:border-accent disabled:bg-raised"
                  value={freeText}
                  onChange={(event) => setFreeText(event.target.value)}
                  disabled={disabled}
                  placeholder="也可以直接在消息输入里自然语言回答"
                />
              </label>
              <button
                className="rounded-md bg-accent px-3 py-2 text-sm font-medium text-white disabled:opacity-40"
                type="submit"
                disabled={disabled || freeText.trim().length === 0}
              >
                提交回答
              </button>
            </form>
          ) : null}

          <p className="text-xs leading-5 text-fg-muted">可点选项，也可直接发消息自然语言回答。</p>
        </div>
      ) : (
        <p className="mt-3 text-sm text-fg-muted">确认项详情会随当前状态查询补齐。</p>
      )}

      {!isPending && answerLabel ? (
        <p className="mt-3 rounded bg-raised px-3 py-2 text-sm text-fg-muted">结果：{answerLabel}</p>
      ) : null}
    </article>
  );
}

function ProgressCard({ item }: { item: ProgressInteractionItem }): ReactElement {
  const percent = item.progress ?? 0;
  return (
    <article className="rounded-md border border-info/40 bg-raised p-4">
      <div className="flex items-center justify-between gap-3">
        <div>
          <p className="text-xs font-medium text-info">进度</p>
          <h3 className="mt-1 text-sm font-semibold text-fg">{item.job_kind}</h3>
        </div>
        <span className="text-sm font-medium text-info">
          {item.progress === null ? "处理中" : `${item.progress}%`}
        </span>
      </div>
      <div
        className="mt-3 h-2 overflow-hidden rounded bg-panel"
        role="progressbar"
        aria-label={`${item.job_kind} 进度`}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={percent}
      >
        <div className="h-full bg-accent" style={{ width: `${percent}%` }} />
      </div>
    </article>
  );
}

function ErrorCard({ item }: { item: ErrorInteractionItem }): ReactElement {
  return (
    <article className="rounded-md border border-danger/40 bg-danger/10 p-4">
      <p className="text-xs font-medium text-danger">错误</p>
      <h3 className="mt-1 text-sm font-semibold text-fg">{item.error_code}</h3>
      <p className="mt-2 text-sm leading-6 text-danger">{item.message}</p>
      <p className="mt-2 text-xs text-danger">{item.retryable ? "可重试" : "不可重试"}</p>
    </article>
  );
}

function PlaceholderCard({
  item,
  label
}: {
  item: PreviewInteractionItem | TimelineInteractionItem;
  label: string;
}): ReactElement {
  return (
    <article className="rounded-md border border-line bg-raised p-4">
      <p className="text-xs font-medium text-fg-muted">{label}</p>
      <h3 className="mt-1 text-sm font-semibold text-fg">{item.title}</h3>
      <p className="mt-2 text-sm leading-6 text-fg-muted">{item.description}</p>
    </article>
  );
}

function UnknownCard({ item }: { item: UnknownInteractionItem }): ReactElement {
  return (
    <details className="rounded-md border border-line bg-panel p-4">
      <summary className="cursor-pointer text-sm font-semibold text-fg">
        未知结构化事件：{item.eventName}
      </summary>
      <pre className="mt-3 max-h-56 overflow-auto rounded bg-raised p-3 text-xs text-fg">
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
  const base = "rounded-md border p-4";
  return isPending
    ? `${base} border-line bg-panel`
    : `${base} border-line bg-raised opacity-75`;
}
