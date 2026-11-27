import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { jsonResponse, renderWithOrg } from '@/test/test-utils';
import { LinksPage } from './LinksPage';

function capabilitiesPayload(overrides?: { analytics?: boolean; public_base_url?: string }) {
  return {
    auth_methods: [{ id: 'oidc', kind: 'redirect', login_url: '/api/v1/auth/oidc/login' }],
    features: {
      analytics: overrides?.analytics ?? true,
      webhooks: true,
      risk_scan: true,
      email: false,
    },
    ...(overrides?.public_base_url ? { public_base_url: overrides.public_base_url } : {}),
  };
}

describe('LinksPage', () => {
  afterEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it('uses public_base_url, localizes ban reason, and locks mutations on banned links', async () => {
    const user = userEvent.setup();
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === '/api/v1/capabilities') {
        return jsonResponse(capabilitiesPayload({ public_base_url: 'https://s.example.com' }));
      }
      if (url.startsWith('/api/v1/links')) {
        return jsonResponse({
          items: [
            {
              code: 'Actv1234',
              url: 'https://example.com/ok',
              status: 'active',
              created_at: '2026-11-23T08:00:00Z',
              clicks: 42,
              clicks_today: 5,
            },
            {
              code: 'Bann9999',
              url: 'https://phish.example/bad',
              status: 'banned',
              ban_reason: 'phishing',
              created_at: '2026-11-23T07:00:00Z',
              clicks: 1,
              clicks_today: 0,
            },
          ],
        });
      }
      throw new Error(`unexpected fetch: ${url}`);
    });

    renderWithOrg(<LinksPage />, { orgId: 9, role: 'owner', linkUsage: 12, linkQuota: 1000 });

    const shortAnchor = await screen.findByRole('link', { name: /s\.example\.com\/r\/Actv1234/ });
    expect(shortAnchor).toHaveAttribute('href', 'https://s.example.com/r/Actv1234');
    expect(screen.getByText(/已封禁 · 钓鱼风险/)).toBeInTheDocument();

    expect(fetchSpy).toHaveBeenCalledWith(
      '/api/v1/links?limit=20&sort=id',
      expect.objectContaining({
        credentials: 'same-origin',
        headers: expect.objectContaining({ 'X-Org-ID': '9' }),
      }),
    );

    await user.click(screen.getByRole('button', { name: '操作 Bann9999' }));
    expect(await screen.findByText('封禁链接不可修改或删除')).toBeInTheDocument();
    expect(screen.getByRole('menuitem', { name: /编辑目标网址/ })).toHaveAttribute('data-disabled');
    expect(screen.getByRole('menuitem', { name: /永久删除/ })).toHaveAttribute('data-disabled');
  });

  it('shows read-only banner and hides write controls and quick-shorten bar for viewer role', async () => {
    const user = userEvent.setup();
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === '/api/v1/capabilities') {
        return jsonResponse(capabilitiesPayload());
      }
      return jsonResponse({
        items: [
          {
            code: 'View1234',
            url: 'https://example.com/read-only',
            status: 'active',
            created_at: '2026-11-23T08:00:00Z',
            clicks: 3,
            clicks_today: 1,
          },
        ],
      });
    });

    renderWithOrg(<LinksPage />, { orgId: 1, role: 'viewer' });

    expect(await screen.findByText('/r/View1234')).toBeInTheDocument();
    expect(screen.getByText(/只读成员权限/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /创建短链/ })).not.toBeInTheDocument();
    expect(screen.queryByLabelText('极速缩短长链接')).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '操作 View1234' }));
    expect(await screen.findByRole('menuitem', { name: /查看二维码/ })).toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: /编辑目标网址/ })).not.toBeInTheDocument();
  });

  it('creates a link via shortcut C and via Quick Shorten Bar', async () => {
    const user = userEvent.setup();
    let count = 0;
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
      const url = String(input);
      if (url === '/api/v1/capabilities') {
        return jsonResponse(capabilitiesPayload({ public_base_url: 'http://127.0.0.1:8080' }));
      }
      if (url === '/api/v1/links' && init?.method === 'POST') {
        count += 1;
        return jsonResponse(
          {
            code: count === 1 ? 'New12345' : 'Qck67890',
            url: count === 1 ? 'https://example.com/fresh' : 'https://example.com/quick',
            status: 'active',
            created_at: '2026-11-23T08:10:00Z',
            clicks: 0,
            clicks_today: 0,
          },
          201,
        );
      }
      if (url.startsWith('/api/v1/links')) {
        return jsonResponse({
          items:
            count > 0
              ? [
                  {
                    code: count === 1 ? 'New12345' : 'Qck67890',
                    url: count === 1 ? 'https://example.com/fresh' : 'https://example.com/quick',
                    status: 'active',
                    created_at: '2026-11-23T08:10:00Z',
                    clicks: 0,
                    clicks_today: 0,
                  },
                ]
              : [],
        });
      }
      throw new Error(`unexpected fetch: ${url}`);
    });

    renderWithOrg(<LinksPage />, { orgId: 3, role: 'member' });

    await screen.findByText('当前组织还没有短链');
    await user.keyboard('c');

    const urlInput = await screen.findByLabelText('目标网址');
    await user.click(screen.getByRole('button', { name: '自定义' }));
    const customExpiryInput = screen.getByLabelText('有效期设置');
    expect(customExpiryInput).toHaveAttribute('type', 'datetime-local');
    expect(
      (customExpiryInput as HTMLInputElement).value > (customExpiryInput as HTMLInputElement).min,
    ).toBe(true);
    await user.click(screen.getByRole('button', { name: '永不过期' }));

    await user.type(urlInput, 'https://example.com/fresh');
    await user.click(screen.getByRole('button', { name: '生成短链' }));

    await waitFor(() => {
      expect(fetchSpy).toHaveBeenCalledWith(
        '/api/v1/links',
        expect.objectContaining({
          method: 'POST',
          body: JSON.stringify({ url: 'https://example.com/fresh' }),
        }),
      );
    });

    expect(await screen.findByText('短链已生成')).toBeInTheDocument();
    expect(screen.getByTestId('created-short-url')).toHaveTextContent(
      'http://127.0.0.1:8080/r/New12345',
    );
    await user.click(screen.getByRole('button', { name: '完成' }));
    expect(await screen.findByText('127.0.0.1:8080/r/New12345')).toBeInTheDocument();

    // 使用顶部极速缩短框快速折叠
    await user.type(screen.getByLabelText('极速缩短长链接'), 'https://example.com/quick');
    await user.click(screen.getByRole('button', { name: /立即折叠/ }));

    expect(await screen.findByText('刚刚折好：')).toBeInTheDocument();
    expect(
      screen.getByRole('link', { name: 'http://127.0.0.1:8080/r/Qck67890' }),
    ).toBeInTheDocument();
  });

  it('deduplicates overlapping items across cursor pages and does not render banned url as link', async () => {
    const user = userEvent.setup();
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === '/api/v1/capabilities') {
        return jsonResponse(capabilitiesPayload());
      }
      if (url.includes('cursor=p2')) {
        return jsonResponse({
          items: [
            {
              code: 'Dup11111',
              url: 'https://example.com/dup',
              status: 'active',
              created_at: '2026-11-23T08:00:00Z',
              clicks: 12,
              clicks_today: 2,
            },
            {
              code: 'Ban22222',
              url: 'https://evil.example/phish',
              status: 'banned',
              ban_reason: 'phishing',
              created_at: '2026-11-23T07:00:00Z',
              clicks: 3,
              clicks_today: 1,
            },
          ],
        });
      }
      return jsonResponse({
        items: [
          {
            code: 'Dup11111',
            url: 'https://example.com/dup',
            status: 'active',
            created_at: '2026-11-23T08:00:00Z',
            clicks: 10,
            clicks_today: 1,
          },
        ],
        next_cursor: 'p2',
      });
    });

    renderWithOrg(<LinksPage />, { orgId: 2, role: 'member' });
    expect(await screen.findByText('/r/Dup11111')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '加载更多短链' }));
    expect(await screen.findByText('https://evil.example/phish')).toBeInTheDocument();
    expect(screen.getAllByText('/r/Dup11111')).toHaveLength(1);
    expect(
      screen.queryByRole('link', { name: 'https://evil.example/phish' }),
    ).not.toBeInTheDocument();
  });

  it('filters links by status pills and debounced server search q, supports exact code bar, and resets on clear', async () => {
    const user = userEvent.setup();
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === '/api/v1/capabilities') {
        return jsonResponse(capabilitiesPayload());
      }
      if (url.includes('q=missing')) {
        return jsonResponse({ items: [] });
      }
      if (url.includes('status=banned') && url.includes('q=phish')) {
        return jsonResponse({
          items: [
            {
              code: 'Ban77777',
              url: 'https://evil.example/phish',
              status: 'banned',
              ban_reason: 'phishing',
              created_at: '2026-11-23T07:00:00Z',
              clicks: 0,
              clicks_today: 0,
            },
          ],
        });
      }
      if (url.includes('status=banned')) {
        return jsonResponse({
          items: [
            {
              code: 'Ban77777',
              url: 'https://evil.example/phish',
              status: 'banned',
              ban_reason: 'phishing',
              created_at: '2026-11-23T07:00:00Z',
              clicks: 0,
              clicks_today: 0,
            },
          ],
        });
      }
      return jsonResponse({
        items: [
          {
            code: 'Actv1234',
            url: 'https://example.com/ok',
            status: 'active',
            created_at: '2026-11-23T08:00:00Z',
            clicks: 5,
            clicks_today: 1,
          },
        ],
      });
    });

    renderWithOrg(<LinksPage />, { orgId: 9, role: 'owner' });
    expect(await screen.findByText('/r/Actv1234')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '已封禁' }));
    await waitFor(() => {
      expect(fetchSpy).toHaveBeenCalledWith(
        '/api/v1/links?limit=20&sort=id&status=banned',
        expect.anything(),
      );
    });
    expect(await screen.findByText('/r/Ban77777')).toBeInTheDocument();

    await user.type(screen.getByLabelText('搜索短链'), 'phish');
    await waitFor(() => {
      expect(fetchSpy).toHaveBeenCalledWith(
        '/api/v1/links?limit=20&sort=id&status=banned&q=phish',
        expect.anything(),
      );
    });

    await user.clear(screen.getByLabelText('搜索短链'));
    await user.type(screen.getByLabelText('搜索短链'), 'AbC12345');
    expect(await screen.findByRole('button', { name: /直达 \/r\/AbC12345/ })).toBeInTheDocument();

    await user.clear(screen.getByLabelText('搜索短链'));
    await user.type(screen.getByLabelText('搜索短链'), 'missing');
    await waitFor(() => {
      expect(fetchSpy).toHaveBeenCalledWith(
        '/api/v1/links?limit=20&sort=id&status=banned&q=missing',
        expect.anything(),
      );
    });
    expect(await screen.findByText('未找到匹配的短链')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '清空筛选' }));
    await waitFor(() => {
      expect(fetchSpy).toHaveBeenLastCalledWith(
        '/api/v1/links?limit=20&sort=id',
        expect.anything(),
      );
    });
    expect(await screen.findByText('/r/Actv1234')).toBeInTheDocument();
  });

  it('hides clicks summary, click sort options, click badge, and stats menu when analytics is disabled', async () => {
    const user = userEvent.setup();
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === '/api/v1/capabilities') {
        return jsonResponse(capabilitiesPayload({ analytics: false }));
      }
      return jsonResponse({
        items: [
          {
            code: 'Lite1234',
            url: 'https://example.com/lite',
            status: 'active',
            created_at: '2026-11-23T08:00:00Z',
          },
        ],
      });
    });

    renderWithOrg(<LinksPage />, { orgId: 1, role: 'owner' });

    expect(await screen.findByText('/r/Lite1234')).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.queryByText(/今日点击/)).not.toBeInTheDocument();
    });
    expect(screen.queryByText('次点击')).not.toBeInTheDocument();
    const sortSelect = screen.getByLabelText('排序方式');
    expect(sortSelect.querySelector('option[value="clicks"]')).toBeNull();
    expect(sortSelect.querySelector('option[value="clicks_today"]')).toBeNull();

    await user.click(screen.getByRole('button', { name: '操作 Lite1234' }));
    expect(await screen.findByRole('menuitem', { name: /查看二维码/ })).toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: /查看点击统计/ })).not.toBeInTheDocument();
  });

  it('conservatively hides click controls while capabilities fails or is pending', async () => {
    const user = userEvent.setup();
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === '/api/v1/capabilities') {
        return jsonResponse({ error: 'internal error' }, 500);
      }
      return jsonResponse({
        items: [
          {
            code: 'Pend1234',
            url: 'https://example.com/pending',
            status: 'active',
            created_at: '2026-11-23T08:00:00Z',
            clicks: 5,
            clicks_today: 1,
          },
        ],
      });
    });

    renderWithOrg(<LinksPage />, { orgId: 1, role: 'owner' });

    expect(await screen.findByText('/r/Pend1234')).toBeInTheDocument();
    expect(screen.queryByText(/今日点击/)).not.toBeInTheDocument();
    expect(screen.queryByText('次点击')).not.toBeInTheDocument();
    const sortSelect = screen.getByLabelText('排序方式');
    expect(sortSelect.querySelector('option[value="clicks"]')).toBeNull();
    expect(sortSelect.querySelector('option[value="clicks_today"]')).toBeNull();

    await user.click(screen.getByRole('button', { name: '操作 Pend1234' }));
    expect(await screen.findByRole('menuitem', { name: /查看二维码/ })).toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: /查看点击统计/ })).not.toBeInTheDocument();
  });
});
