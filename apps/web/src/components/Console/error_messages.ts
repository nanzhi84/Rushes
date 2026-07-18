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

export function resendErrorMessage(error: unknown): string {
  const reason = timelinePatchErrorMessage(error);
  if (reason === "resend_cancellation_timeout") {
    return "当前任务尚未安全停止，请稍后重试。";
  }
  if (reason === "resend_checkpoint_unavailable") {
    return "这条消息太早了，已无法回到它之前的状态。";
  }
  if (reason === "resend_message_not_editable") {
    return "这条消息已被新的编辑覆盖，请刷新后重试。";
  }
  if (reason === "resend_job_state_changed") {
    return "任务状态刚刚发生变化，请稍后重试。";
  }
  if (reason === "resend_idempotency_key_reused") {
    return "这次重发的参数已变化，请重新操作。";
  }
  if (reason === "turn_queue_closed") {
    return "剪辑任务队列已停止，请重启本地服务后再重发。";
  }
  if (reason === "empty_message") {
    return "消息内容不能为空。";
  }
  if (reason === "version_conflict" || reason === "API 请求失败：409") {
    return "草稿刚刚发生了变化，请刷新后重试。";
  }
  return `编辑重发失败：${reason}`;
}
