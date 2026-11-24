import { afterEach, describe, expect, it, vi } from 'vitest';
import { jsonResponse } from '@/test/test-utils';
import { APIError, apiRequest, getErrorMessage, setUnauthorizedHandler } from './api-client';
import { ENDPOINTS } from './endpoints';

describe('apiRequest', () => {
  afterEach(() => {
    setUnauthorizedHandler(null);
    vi.restoreAllMocks();
  });

  it('sends credentials, X-Org-ID header, and JSON body', async () => {
    const fetchSpy = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValue(jsonResponse({ ok: true }, 200));

    const result = await apiRequest<{ ok: boolean }>(ENDPOINTS.links.listCreate, {
      method: 'POST',
      orgId: 42,
      body: { url: 'https://example.com' },
    });

    expect(result).toEqual({ ok: true });
    expect(fetchSpy).toHaveBeenCalledWith(
      ENDPOINTS.links.listCreate,
      expect.objectContaining({
        method: 'POST',
        credentials: 'same-origin',
        headers: {
          'X-Org-ID': '42',
          'Content-Type': 'application/json',
        },
        body: JSON.stringify({ url: 'https://example.com' }),
      }),
    );
  });

  it('returns undefined on 204 No Content', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse(null, 204));

    const result = await apiRequest<void>(ENDPOINTS.links.detail('AbCd1234'), {
      method: 'DELETE',
      orgId: 1,
    });

    expect(result).toBeUndefined();
  });

  it('triggers onUnauthorized on 401 by default and translates APIError', async () => {
    const onUnauthorized = vi.fn();
    setUnauthorizedHandler(onUnauthorized);
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse({ error: 'unauthorized' }, 401));

    let caught: unknown;
    try {
      await apiRequest(ENDPOINTS.links.listCreate, { orgId: 1 });
    } catch (err) {
      caught = err;
    }

    expect(onUnauthorized).toHaveBeenCalledTimes(1);
    expect(caught).toBeInstanceOf(APIError);
    const apiErr = caught as APIError;
    expect(apiErr.status).toBe(401);
    expect(apiErr.rawMessage).toBe('unauthorized');
    expect(apiErr.message).toBe('登录状态已失效，请重新登录');
    expect(getErrorMessage(apiErr)).toBe('登录状态已失效，请重新登录');
  });

  it('does not trigger onUnauthorized when skipAuthRedirect is true', async () => {
    const onUnauthorized = vi.fn();
    setUnauthorizedHandler(onUnauthorized);
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse({ error: 'unauthorized' }, 401));

    await expect(apiRequest(ENDPOINTS.auth.me, { skipAuthRedirect: true })).rejects.toMatchObject({
      status: 401,
      rawMessage: 'unauthorized',
    });

    expect(onUnauthorized).not.toHaveBeenCalled();
  });

  it('falls back to HTTP status message when response body is non-JSON', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response('Bad Gateway', { status: 502 }));

    await expect(apiRequest(ENDPOINTS.orgs.listCreate)).rejects.toMatchObject({
      status: 502,
      rawMessage: 'HTTP 502',
    });
  });
});
