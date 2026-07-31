import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Download, RotateCcw } from "lucide-react";
import { useMemo, useState, type ReactElement } from "react";
import { api, type UserExportRecord, type UserExportsResponse } from "../../api/client";
import { queryKeys } from "../../app/query_client";

type ExportOrientation = "auto" | "portrait" | "landscape";

type ExportControlProps = {
  draftId: string;
  timelineId: string | null;
  timelineVersion: number | null;
  disabled: boolean;
  disabledReason?: string;
};

export function ExportControl({
  draftId,
  timelineId,
  timelineVersion,
  disabled,
  disabledReason
}: ExportControlProps): ReactElement {
  const queryClient = useQueryClient();
  const [orientation, setOrientation] = useState<ExportOrientation>("auto");
  const exportsQuery = useQuery({
    queryKey: queryKeys.exports(draftId),
    queryFn: () => api.listUserExports(draftId),
    refetchInterval: (query) =>
      query.state.data?.exports?.some(isActiveExport) ? 1_000 : false
  });
  const records = exportsQuery.data?.exports ?? [];
  const matchingRecord = useMemo(
    () =>
      records.find(
        (record) => record.timeline_id === timelineId && record.orientation === orientation
      ) ?? null,
    [orientation, records, timelineId]
  );
  const activeRecord = records.find(isActiveExport) ?? null;
  const historicalSucceeded =
    records.find(
      (record) =>
        record.status === "succeeded" &&
        record.export_id !== null &&
      record.timeline_id !== timelineId
    ) ?? null;
  const historicalRetryable =
    records.find((record) => record.retryable && record.timeline_id !== timelineId) ?? null;
  const displayedRecord =
    matchingRecord ?? activeRecord ?? historicalRetryable ?? historicalSucceeded;
  const downloadRecord =
    matchingRecord?.status === "succeeded" && matchingRecord.export_id
      ? matchingRecord
      : historicalSucceeded;
  const retryRecord = matchingRecord?.retryable ? matchingRecord : historicalRetryable;

  const updateRecord = (record: UserExportRecord): void => {
    queryClient.setQueryData<UserExportsResponse>(queryKeys.exports(draftId), (current) => ({
      exports: [
        record,
        ...(current?.exports.filter((item) => item.job_id !== record.job_id) ?? [])
      ]
    }));
  };
  const createMutation = useMutation({
    mutationFn: () =>
      api.createUserExport(draftId, {
        timeline_id: timelineId ?? "",
        orientation
      }),
    onSuccess: updateRecord
  });
  const retryMutation = useMutation({
    mutationFn: (jobId: string) => api.retryUserExport(draftId, jobId),
    onSuccess: updateRecord
  });

  const mutationPending = createMutation.isPending || retryMutation.isPending;
  const active = activeRecord !== null;
  const actionDisabled = disabled || timelineId === null || mutationPending || active;
  const requestFailed = createMutation.isError || retryMutation.isError;

  return (
    <div className="flex min-w-0 items-center gap-1.5" aria-label="最终导出">
      <label className="sr-only" htmlFor={`export-orientation-${draftId}`}>
        导出画幅
      </label>
      <select
        id={`export-orientation-${draftId}`}
        aria-label="导出画幅"
        className="h-7 rounded-sm border border-line-strong bg-panel px-1.5 text-2xs text-fg outline-none focus:border-accent disabled:opacity-40"
        value={orientation}
        disabled={disabled || mutationPending || active}
        onChange={(event) => setOrientation(event.target.value as ExportOrientation)}
      >
        <option value="auto">自动画幅</option>
        <option value="portrait">竖屏</option>
        <option value="landscape">横屏</option>
      </select>

      {historicalSucceeded && displayedRecord === historicalSucceeded && timelineVersion ? (
        <span
          className="max-w-44 truncate text-2xs text-fg-muted"
          role="status"
          title={`已有导出来自时间线 v${historicalSucceeded.timeline_version}；当前时间线为 v${timelineVersion}`}
        >
          已有 v{historicalSucceeded.timeline_version} 成片，当前 v{timelineVersion}
        </span>
      ) : displayedRecord ? (
        <ExportStatus record={displayedRecord} />
      ) : null}
      {requestFailed ? (
        <span className="max-w-32 truncate text-2xs text-danger" role="alert">
          导出请求失败，请重试
        </span>
      ) : null}

      {downloadRecord?.export_id ? (
        <a
          className="inline-flex h-7 items-center gap-1 whitespace-nowrap rounded-sm bg-accent px-2.5 text-xs font-semibold text-white hover:bg-accent-strong active:translate-y-px"
          href={api.mediaExportUrl(downloadRecord.export_id)}
          download={`rushes-v${downloadRecord.timeline_version}.mp4`}
        >
          <Download size={12} strokeWidth={1.75} aria-hidden />
          下载 v{downloadRecord.timeline_version}
        </a>
      ) : null}
      {retryRecord ? (
        <button
          className="inline-flex h-7 items-center gap-1 whitespace-nowrap rounded-sm border border-line-strong px-2.5 text-xs font-semibold text-fg hover:bg-hover active:translate-y-px disabled:opacity-40"
          type="button"
          disabled={disabled || mutationPending || active}
          onClick={() => retryMutation.mutate(retryRecord.job_id)}
        >
          <RotateCcw size={12} strokeWidth={1.75} aria-hidden />
          重试 v{retryRecord.timeline_version}
        </button>
      ) : null}
      {matchingRecord?.status === "succeeded" || matchingRecord?.retryable ? null : (
        <button
          className="h-7 whitespace-nowrap rounded-sm bg-accent px-3 text-xs font-semibold text-white hover:bg-accent-strong active:translate-y-px disabled:opacity-40"
          type="button"
          disabled={actionDisabled}
          title={actionDisabled ? disabledReason : undefined}
          onClick={() => createMutation.mutate()}
        >
          {mutationPending ? "提交中" : timelineVersion ? `导出 v${timelineVersion}` : "导出"}
        </button>
      )}
    </div>
  );
}

function ExportStatus({ record }: { record: UserExportRecord }): ReactElement {
  if (record.status === "pending") {
    return <span className="whitespace-nowrap text-2xs text-fg-muted">v{record.timeline_version} 已排队</span>;
  }
  if (record.status === "running") {
    return (
      <span className="whitespace-nowrap text-2xs tabular-nums text-fg-muted" role="status">
        v{record.timeline_version} 导出中 {Math.round(record.progress * 100)}%
      </span>
    );
  }
  if (record.status === "failed" || record.status === "cancelled") {
    return (
      <span
        className="max-w-36 truncate text-2xs text-danger"
        role="status"
        title={record.error?.message}
      >
        v{record.timeline_version} {record.status === "cancelled" ? "已取消" : "导出失败"}
      </span>
    );
  }
  return <span className="whitespace-nowrap text-2xs text-fg-muted">v{record.timeline_version} 已完成</span>;
}

function isActiveExport(record: UserExportRecord): boolean {
  return record.status === "pending" || record.status === "running";
}
