import { ApiError } from "../../auth";

// 从后端错误里提取可读 reason：优先取 ApiError.payload.detail.reason，否则回退到
// error.message。时间线写路径与会话（清空/取消任务/回退）共用这一层，故抽成叶子模块，
// 避免 DraftEditorView 与 ConsolePanel 之间产生循环依赖。
export function timelinePatchErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.payload && typeof error.payload === "object") {
    const detail = Reflect.get(error.payload, "detail");
    if (detail && typeof detail === "object") {
      const reason = Reflect.get(detail, "reason");
      if (typeof reason === "string" && reason.trim()) {
        return reason;
      }
    }
  }
  return error instanceof Error ? error.message : "时间线修改失败";
}

export function conversationClearErrorMessage(error: unknown): string {
  const reason = timelinePatchErrorMessage(error);
  if (reason === "turn_active") {
    return "当前任务仍在运行，请先停止或等待本轮结束后再清空对话。";
  }
  return reason === "API 请求失败：409" ? "当前任务仍在运行，暂时不能清空对话。" : reason;
}

export function jobCancelErrorMessage(error: unknown): string {
  const reason = timelinePatchErrorMessage(error);
  if (reason === "job_not_cancellable" || reason === "API 请求失败：409") {
    return "任务状态已变化，无法取消；已刷新当前状态。";
  }
  return `取消任务失败：${reason}`;
}

export function rewindErrorMessage(error: unknown): string {
  const reason = timelinePatchErrorMessage(error);
  if (reason === "rewind_checkpoint_not_found") {
    return "检查点已被清理，请刷新后选择新的检查点。";
  }
  if (reason === "rewind_checkpoint_has_no_timeline") {
    return "这个检查点没有可恢复的时间线，请改用仅对话。";
  }
  if (reason === "rewind_cancellation_timeout") {
    return "当前任务尚未安全停止，请稍后重试恢复。";
  }
  if (reason === "turn_queue_closed") {
    return "剪辑任务队列已停止，请重启本地服务后再恢复。";
  }
  if (reason === "rewind_idempotency_key_reused") {
    return "这次恢复请求已用于另一个检查点，请重新操作。";
  }
  if (reason === "version_conflict" || reason === "API 请求失败：409") {
    return "草稿刚刚发生了变化，请刷新检查点后重试。";
  }
  return `恢复检查点失败：${reason}`;
}
