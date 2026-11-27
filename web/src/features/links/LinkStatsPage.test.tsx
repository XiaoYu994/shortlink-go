import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Route, Routes } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ROUTE_PATTERNS, routes } from '@/app/routes';
import { jsonResponse, renderWithOrg } from '@/test/test-utils';
import { LinkStatsPage } from './LinkStatsPage';

function renderStatsPage(orgId = 1, code = 'AbC12345') {
  return renderWithOrg(
    <Routes>
      <Route path={ROUTE_PATTERNS.orgLinkStats} element={<LinkStatsPage />} />
    </Routes>,
    { orgId, route: routes.linkStats(orgId, code) },
  );
}

describe('LinkStatsPage', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('shows disabled notice and does not fetch stats when analytics is disabled', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === '/api/v1/capabilities') {
        return jsonResponse({
          auth_methods: [{ id: 'oidc', kind: 'redirect', login_url: '/api/v1/auth/oidc/login' }],
          features: { analytics: false, webhooks: true, risk_scan: true, email: false },
        });
      }
      if (url === '/api/v1/links/AbC12345') {
        return jsonResponse({
          code: 'AbC12345',
          url: 'https://example.com/landing',
          status: 'active',
          created_at: '2026-11-25T08:00:00Z',
        });
      }
      throw new Error(`unexpected fetch: ${url}`);
    });

    renderStatsPage(1, 'AbC12345');

    expect(await screen.findByText('访问统计暂未开启')).toBeInTheDocument();
    expect(fetchSpy.mock.calls.some(([input]) => String(input).includes('/stats'))).toBe(false);
  });

  it('recovers capabilities error, localizes breakdown labels, supports 90d range and in-page disable', async () => {
    const user = userEvent.setup();
    let failCapabilities = true;
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
      const url = String(input);
      if (url === '/api/v1/capabilities') {
        if (failCapabilities) {
          return jsonResponse({ error: 'bad gateway' }, 502);
        }
        return jsonResponse({
          auth_methods: [{ id: 'oidc', kind: 'redirect', login_url: '/api/v1/auth/oidc/login' }],
          features: { analytics: true, webhooks: true, risk_scan: true, email: false },
        });
      }
      if (url === '/api/v1/links/AbC12345' && init?.method === 'PATCH') {
        return jsonResponse({
          code: 'AbC12345',
          url: 'https://example.com/landing',
          status: 'disabled',
          created_at: '2026-11-25T08:00:00Z',
        });
      }
      if (url === '/api/v1/links/AbC12345') {
        return jsonResponse({
          code: 'AbC12345',
          url: 'https://example.com/landing',
          status: 'active',
          created_at: '2026-11-25T08:00:00Z',
          clicks: 18,
          clicks_today: 4,
        });
      }
      if (url.startsWith('/api/v1/links/AbC12345/stats')) {
        return jsonResponse({
          code: 'AbC12345',
          total_clicks: 18,
          range_clicks: 12,
          from: '2026-11-18T00:00:00Z',
          to: '2026-11-25T00:00:00Z',
          clicks: [{ day: '2026-11-25', clicks: 4 }],
          referrers: [{ referrer: 'direct', clicks: 12 }],
          devices: [
            { device: 'desktop', clicks: 12 },
            { device: 'mobile', clicks: 0 },
          ],
          clients: [{ client: 'browser', clicks: 12 }],
          daily_visitors: [{ day: '2026-11-25', visitors: 3 }],
          unique_visitors: 9,
        });
      }
      throw new Error(`unexpected fetch: ${url}`);
    });

    renderStatsPage(1, 'AbC12345');

    expect(await screen.findByText(/功能状态加载失败/)).toBeInTheDocument();
    expect(fetchSpy.mock.calls.some(([input]) => String(input).includes('/stats'))).toBe(false);

    failCapabilities = false;
    await user.click(screen.getByRole('button', { name: '重试' }));
    expect(await screen.findByText('历史总点击')).toBeInTheDocument();
    expect(screen.getByText('18')).toBeInTheDocument();
    expect(screen.getByText('直接访问 (direct)')).toBeInTheDocument();
    expect(screen.getByText('桌面端')).toBeInTheDocument();
    expect(screen.queryByText('移动端')).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '近 90 天' }));
    await waitFor(() => {
      const statsCalls = fetchSpy.mock.calls.filter(([input]) => String(input).includes('/stats'));
      expect(statsCalls.length).toBeGreaterThanOrEqual(2);
    });

    await user.click(screen.getByRole('button', { name: '停用' }));
    await waitFor(() => {
      expect(fetchSpy).toHaveBeenCalledWith(
        '/api/v1/links/AbC12345',
        expect.objectContaining({
          method: 'PATCH',
          body: JSON.stringify({ disabled: true }),
        }),
      );
    });
  });
});
