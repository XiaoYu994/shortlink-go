import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { jsonResponse, renderWithOrg } from '@/test/test-utils';
import { LinksPage } from './LinksPage';

describe('LinksPage', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('uses public_base_url for short link href and locks mutations on banned links', async () => {
    const user = userEvent.setup();
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === '/api/v1/auth/providers') {
        return jsonResponse({
          oidc: true,
          github: false,
          public_base_url: 'https://s.example.com',
        });
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
    expect(screen.getByText(/已封禁 · phishing/)).toBeInTheDocument();

    expect(fetchSpy).toHaveBeenCalledWith(
      '/api/v1/links?limit=20&sort=id',
      expect.objectContaining({
        credentials: 'same-origin',
        headers: expect.objectContaining({ 'X-Org-ID': '9' }),
      }),
    );

    await user.click(screen.getByRole('button', { name: '操作 Bann9999' }));
    expect(await screen.findByText('封禁链接不可修改或删除')).toBeInTheDocument();
    expect(screen.getByRole('menuitem', { name: /编辑目标 URL/ })).toHaveAttribute('data-disabled');
    expect(screen.getByRole('menuitem', { name: /永久删除/ })).toHaveAttribute('data-disabled');
  });

  it('shows read-only banner and hides write controls for viewer role', async () => {
    const user = userEvent.setup();
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === '/api/v1/auth/providers') {
        return jsonResponse({ oidc: true, github: false });
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
    expect(screen.getByText(/只读观察者（Viewer）/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /创建短链/ })).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '操作 View1234' }));
    expect(await screen.findByRole('menuitem', { name: /查看二维码/ })).toBeInTheDocument();
    expect(screen.queryByRole('menuitem', { name: /编辑目标 URL/ })).not.toBeInTheDocument();
  });

  it('creates a link via shortcut C and shows copy + QR code success state', async () => {
    const user = userEvent.setup();
    let created = false;
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
      const url = String(input);
      if (url === '/api/v1/auth/providers') {
        return jsonResponse({
          oidc: true,
          github: false,
          public_base_url: 'http://127.0.0.1:8080',
        });
      }
      if (url === '/api/v1/links' && init?.method === 'POST') {
        created = true;
        return jsonResponse(
          {
            code: 'New12345',
            url: 'https://example.com/fresh',
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
          items: created
            ? [
                {
                  code: 'New12345',
                  url: 'https://example.com/fresh',
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

    const urlInput = await screen.findByLabelText('目标 URL');
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

    // 创建成功后停留在成功态，展示公开域名短链与一键复制按钮。
    expect(await screen.findByText('短链已生成')).toBeInTheDocument();
    expect(screen.getByTestId('created-short-url')).toHaveTextContent(
      'http://127.0.0.1:8080/r/New12345',
    );
    await user.click(screen.getByRole('button', { name: '完成' }));
    expect(await screen.findByText('127.0.0.1:8080/r/New12345')).toBeInTheDocument();
  });
});
