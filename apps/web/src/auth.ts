export const AUTH_TOKEN_STORAGE_KEY = "rushes.launch_token";
export const AUTH_REQUIRED_EVENT = "rushes:auth-required";
export const AUTH_CHANGED_EVENT = "rushes:auth-changed";

export class ApiError extends Error {
  readonly status: number;
  readonly payload: unknown;

  constructor(status: number, message: string, payload: unknown = null) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.payload = payload;
  }
}

export type ApiFetchOptions = Omit<RequestInit, "body" | "headers"> & {
  body?: unknown;
  headers?: HeadersInit;
};

export function bootstrapAuthFromLaunchUrl(
  location: Location = window.location,
  history: History = window.history
): void {
  const token = tokenFromHash(location.hash);
  if (!token) {
    return;
  }
  storeAuthToken(token);
  history.replaceState(null, document.title, `${location.pathname}${location.search}`);
}

export function listenForLaunchUrlAuth(
  target: Window = window,
  location: Location = target.location,
  history: History = target.history
): () => void {
  const handleHashChange = () => bootstrapAuthFromLaunchUrl(location, history);
  target.addEventListener("hashchange", handleHashChange);
  return () => target.removeEventListener("hashchange", handleHashChange);
}

export function getAuthToken(): string | null {
  const persisted = readStorage("localStorage");
  if (persisted) {
    return persisted;
  }
  const sessionToken = readStorage("sessionStorage");
  if (!sessionToken) {
    return null;
  }
  try {
    window.localStorage.setItem(AUTH_TOKEN_STORAGE_KEY, sessionToken);
    window.sessionStorage.removeItem(AUTH_TOKEN_STORAGE_KEY);
  } catch {
    // localStorage 不可用时仍保留旧会话的 token。
  }
  return sessionToken;
}

export function storeAuthToken(token: string): void {
  try {
    window.localStorage.setItem(AUTH_TOKEN_STORAGE_KEY, token);
    window.sessionStorage.removeItem(AUTH_TOKEN_STORAGE_KEY);
  } catch {
    try {
      window.sessionStorage.setItem(AUTH_TOKEN_STORAGE_KEY, token);
    } catch {
      return;
    }
  }
  window.dispatchEvent(new Event(AUTH_CHANGED_EVENT));
}

function clearAuthToken(): void {
  try {
    window.localStorage.removeItem(AUTH_TOKEN_STORAGE_KEY);
  } catch {
    // 仍需尝试清理会话级的旧 token。
  }
  try {
    window.sessionStorage.removeItem(AUTH_TOKEN_STORAGE_KEY);
  } catch {
    // 两种存储均不可用时也要通知 UI 刷新认证态。
  }
  window.dispatchEvent(new Event(AUTH_CHANGED_EVENT));
}

function readStorage(storageName: "localStorage" | "sessionStorage"): string | null {
  try {
    return window[storageName].getItem(AUTH_TOKEN_STORAGE_KEY);
  } catch {
    return null;
  }
}

export async function apiFetch<T>(path: string, options: ApiFetchOptions = {}): Promise<T> {
  const headers = withAuthHeaders(options.headers);
  const hasBody = options.body !== undefined;
  if (hasBody && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }

  const response = await fetch(path, {
    ...options,
    headers,
    body: hasBody ? JSON.stringify(options.body) : undefined
  });

  if (response.status === 401) {
    handleUnauthorized();
  }

  if (!response.ok) {
    throw new ApiError(response.status, `API 请求失败：${response.status}`, await readPayload(response));
  }

  if (response.status === 204) {
    return undefined as T;
  }

  const text = await response.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

function createApiEventSource(path: string): EventSource {
  const token = getAuthToken();
  if (!token) {
    handleUnauthorized();
    throw new ApiError(401, "缺少启动 token");
  }
  const url = new URL(path, window.location.origin);
  url.searchParams.set("token", token);
  if (/^\/api\/drafts\/[^/]+\/(?:events|turn-stream)$/.test(url.pathname)) {
    url.searchParams.set("turn_stream_client_id", getTurnStreamClientId(url.pathname));
  }
  return new EventSource(url.toString());
}

const turnStreamClientIds = new Map<string, string>();

function getTurnStreamClientId(pathname: string): string {
  const existing = turnStreamClientIds.get(pathname);
  if (existing) {
    return existing;
  }
  const clientId =
    typeof crypto.randomUUID === "function"
      ? crypto.randomUUID()
      : `${Date.now()}-${Math.random().toString(36).slice(2)}`;
  turnStreamClientIds.set(pathname, clientId);
  return clientId;
}

type SharedEventSourceEntry = {
  source: EventSource;
  refCount: number;
};

const sharedEventSources = new Map<string, SharedEventSourceEntry>();

/**
 * 按 path 引用计数共享 EventSource：多个订阅方（顶栏连接态、素材面板）复用同一条
 * SSE 长连接，避免打满浏览器 HTTP/1.1 同源连接池。订阅方用 addEventListener
 * 挂监听（不要写 onopen/onerror 独占槽位），release 归零时关闭连接。
 */
export function acquireApiEventSource(path: string): {
  source: EventSource;
  release: () => void;
} {
  const existing = sharedEventSources.get(path);
  if (existing) {
    existing.refCount += 1;
    return { source: existing.source, release: () => releaseApiEventSource(path) };
  }
  const source = createApiEventSource(path);
  sharedEventSources.set(path, { source, refCount: 1 });
  return { source, release: () => releaseApiEventSource(path) };
}

function releaseApiEventSource(path: string): void {
  const entry = sharedEventSources.get(path);
  if (!entry) {
    return;
  }
  entry.refCount -= 1;
  if (entry.refCount <= 0) {
    entry.source.close();
    sharedEventSources.delete(path);
  }
}

function handleUnauthorized(): void {
  clearAuthToken();
  if (window.location.pathname !== "/") {
    window.history.pushState(null, document.title, "/");
  }
  window.dispatchEvent(new Event(AUTH_REQUIRED_EVENT));
}

function withAuthHeaders(input: HeadersInit | undefined): Headers {
  const headers = new Headers(input);
  const token = getAuthToken();
  if (token) {
    headers.set("Authorization", `Bearer ${token}`);
  }
  return headers;
}

function tokenFromHash(hash: string): string | null {
  const raw = hash.startsWith("#") ? hash.slice(1) : hash;
  if (!raw) {
    return null;
  }
  const params = new URLSearchParams(raw);
  const token = params.get("t");
  return token && token.trim().length > 0 ? token : null;
}

async function readPayload(response: Response): Promise<unknown> {
  const text = await response.text();
  if (!text) {
    return null;
  }
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}
