/*
  api-client 封装所有发往 Go console 后端的同源 HTTP 请求（类似 Go 里的 http.Client 封装）。
  - 始终携带 credentials: 'same-origin'，让浏览器自动附带 HttpOnly 的 sl_session Cookie；
  - 支持 AbortSignal（由 TanStack Query 传入），切换组织或组件卸载时自动取消在途请求，避免竞态覆盖；
  - 统一把非 2xx 响应解析成强类型的 APIError，方便上层按 status 区分 401/403/404/409。
*/

export class APIError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = 'APIError';
    this.status = status;
  }
}

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PATCH' | 'DELETE';
  orgId?: number;
  body?: unknown;
  signal?: AbortSignal;
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
    const message =
      parsed &&
      typeof parsed === 'object' &&
      'error' in parsed &&
      typeof parsed.error === 'string'
        ? parsed.error
        : `HTTP ${res.status}`;
    throw new APIError(res.status, message);
  }

  return parsed as T;
}

export function getErrorMessage(err: unknown): string {
  if (err instanceof APIError) return err.message;
  if (err instanceof Error) return err.message;
  return '请求失败，请稍后重试';
}
