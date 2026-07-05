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

export function getAuthToken(): string | null {
  try {
    return window.sessionStorage.getItem(AUTH_TOKEN_STORAGE_KEY);
  } catch {
    return null;
  }
}

export function storeAuthToken(token: string): void {
  try {
    window.sessionStorage.setItem(AUTH_TOKEN_STORAGE_KEY, token);
  } catch {
    return;
  }
  window.dispatchEvent(new Event(AUTH_CHANGED_EVENT));
}

export function clearAuthToken(): void {
  try {
    window.sessionStorage.removeItem(AUTH_TOKEN_STORAGE_KEY);
  } catch {
    return;
  }
  window.dispatchEvent(new Event(AUTH_CHANGED_EVENT));
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

export function createApiEventSource(path: string): EventSource {
  const token = getAuthToken();
  if (!token) {
    handleUnauthorized();
    throw new ApiError(401, "缺少启动 token");
  }
  const url = new URL(path, window.location.origin);
  url.searchParams.set("token", token);
  return new EventSource(url.toString());
}

export function handleUnauthorized(): void {
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
