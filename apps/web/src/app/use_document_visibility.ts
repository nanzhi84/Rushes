import { useEffect, useState } from "react";

function documentIsVisible(): boolean {
  return typeof document === "undefined" || document.visibilityState !== "hidden";
}

/**
 * 浏览器对同一 origin 的 HTTP/1.1 长连接数量有限。编辑器会同时使用 workspace、
 * draft 与 turn-stream 三条 SSE，因此后台标签页必须主动释放连接；重新切回时 SSE
 * 会从持久化事件/回合快照恢复，不会丢状态。
 */
export function useDocumentVisibility(): boolean {
  const [visible, setVisible] = useState(documentIsVisible);

  useEffect(() => {
    const handleVisibilityChange = () => setVisible(documentIsVisible());
    document.addEventListener("visibilitychange", handleVisibilityChange);
    handleVisibilityChange();
    return () => document.removeEventListener("visibilitychange", handleVisibilityChange);
  }, []);

  return visible;
}
