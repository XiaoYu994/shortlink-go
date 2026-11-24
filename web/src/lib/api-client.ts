import { translateAPIError } from './error-messages';

/*
  api-client 是零业务路径依赖的通用同源 HTTP 客户端（类比 Go 的 internal/httpx）。
  - 始终携带 credentials: 'same-origin'，自动附带 HttpOnly 会话 Cookie；
  - 支持 AbortSignal，切换组织或卸载组件时自动取消在途请求；
  - 是否跳过全局 401 回跳由调用方通过 skipAuthRedirect 显式声明，底层绝不写死业务路径字符串。
*/

export class APIError extends Error {
  readonly status: number;
  readonly rawMessage: string;

  constructor(status: number, rawMessage: string) {
    super(translateAPIError(rawMessage, status));
    this.name = 'APIError';
    this.status = status;
    this.rawMessage = rawMessage;
  }
}

type UnauthorizedHandler = () => void;
let onUnauthorized: UnauthorizedHandler | null = null;

export function setUnauthorizedHandler(handler: UnauthorizedHandler | null): void {
  onUnauthorized = handler;
}

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PATCH' | 'DELETE';
  orgId?: number;
  body?: unknown;
  signal?: AbortSignal;
  // 为 true 时收到 401 不触发全局登出跳转（例如 /me 初始探测或登出端点自身）。
  skipAuthRedirect?: boolean;
}

export async function apiRequest<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const headers: Record<string, string> = {};
  if (opts.orgId !== undefined) {
    headers['X-Org-ID'] = String(opts.orgId);
  }

  let body: string | undefined;
  if (opts.body !== undefined) {
    headers['Content-Type'] = 'application/json';
    body = JSON.stringify(opts.body);
  }

  const res = await fetch(path, {
    method: opts.method ?? 'GET',
    credentials: 'same-origin',
    headers,
    body,
    signal: opts.signal,
  });

  if (res.status === 204) {
    return undefined as T;
  }

  const text = await res.text();
  let parsed: unknown = undefined;
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = undefined;
    }
  }

  if (!res.ok) {
    const rawMessage =
      parsed && typeof parsed === 'object' && 'error' in parsed && typeof parsed.error === 'string'
        ? parsed.error
        : `HTTP ${res.status}`;

    if (res.status === 401 && !opts.skipAuthRedirect && onUnauthorized) {
      onUnauthorized();
    }

    throw new APIError(res.status, rawMessage);
  }

  return parsed as T;
}

export function getErrorMessage(err: unknown): string {
  if (err instanceof APIError) return err.message;
  if (err instanceof Error) return translateAPIError(err.message);
  return '请求失败，请稍后重试';
}
