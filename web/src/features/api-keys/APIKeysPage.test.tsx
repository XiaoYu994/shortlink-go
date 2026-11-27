import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { jsonResponse, renderWithOrg } from '@/test/test-utils';
import { APIKeysPage } from './APIKeysPage';

describe('APIKeysPage', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('blocks member role without firing /api/v1/api-keys request', () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch');

    renderWithOrg(<APIKeysPage />, { orgId: 2, role: 'member' });

    expect(screen.getByText('仅所有者与管理员可管理 API Key')).toBeInTheDocument();
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('issues read-only key by default with GET curl, masks secret until revealed, and guards dismiss', async () => {
    const user = userEvent.setup();
    const plaintext = 'slk_' + 'a'.repeat(64);
    let issued = false;

    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
      const url = String(input);
      if (url === '/api/v1/members') {
        return jsonResponse({
          items: [
            {
              user_id: 7,
              email: 'alice@example.com',
              name: 'Alice',
              role: 'owner',
              joined_at: '2026-11-23T07:00:00Z',
            },
          ],
        });
      }
      if (url === '/api/v1/api-keys' && init?.method === 'POST') {
        issued = true;
        return jsonResponse(
          {
            id: 11,
            name: 'ci-reader',
            scopes: ['links:read'],
            status: 'active',
            created_by: 7,
            created_at: '2026-11-23T08:00:00Z',
            api_key: plaintext,
          },
          201,
        );
      }
      if (url === '/api/v1/api-keys') {
        return jsonResponse({
          items: issued
            ? [
                {
                  id: 11,
                  name: 'ci-reader',
                  scopes: ['links:read'],
                  status: 'active',
                  created_by: 7,
                  created_at: '2026-11-23T08:00:00Z',
                  last_used_at: '2026-11-24T09:00:00Z',
                },
              ]
            : [],
        });
      }
      throw new Error(`unexpected fetch: ${url}`);
    });

    renderWithOrg(<APIKeysPage />, { orgId: 5, role: 'owner' });

    await screen.findByText('还没有组织 API Key');
    await user.click(screen.getByRole('button', { name: /创建第一把 API Key/ }));
    expect(await screen.findByRole('button', { name: '只读（推荐）' })).toHaveAttribute(
      'aria-pressed',
      'true',
    );
    await user.type(screen.getByLabelText('名称 / 备注（可选）'), 'ci-reader');
    await user.click(screen.getByRole('button', { name: '确认创建' }));

    await waitFor(() => {
      expect(fetchSpy).toHaveBeenCalledWith(
        '/api/v1/api-keys',
        expect.objectContaining({
          method: 'POST',
          body: JSON.stringify({
            name: 'ci-reader',
            scopes: ['links:read'],
          }),
        }),
      );
    });

    // 默认掩码防泄露，点击显示明文才暴露完整字符串。
    const secretNode = await screen.findByTestId('issued-api-key-secret');
    expect(secretNode).not.toHaveTextContent(plaintext);
    await user.click(screen.getByRole('button', { name: '显示明文密钥' }));
    expect(secretNode).toHaveTextContent(plaintext);

    // 仅含 links:read 权限时动态生成 GET 示例，且默认使用环境变量占位符。
    const curlNode = screen.getByTestId('issued-api-key-curl');
    expect(curlNode).toHaveTextContent('/api/v1/links?limit=10');
    expect(curlNode).toHaveTextContent('Authorization: Bearer $ORI_API_KEY');
    await user.click(screen.getByLabelText('在示例命令中填入本次明文'));
    expect(curlNode).toHaveTextContent(`Authorization: Bearer ${plaintext}`);

    // 未勾选确认前禁止关闭一次性密钥横幅。
    const dismissBtn = screen.getByRole('button', { name: '我已保存，关闭' });
    expect(dismissBtn).toBeDisabled();
    await user.click(screen.getByLabelText(/我已安全复制并保存该密钥/));
    expect(dismissBtn).toBeEnabled();

    expect(await screen.findByText('ci-reader')).toBeInTheDocument();
    expect(screen.getByText('#11')).toBeInTheDocument();
    expect(screen.getByText(/最近使用/)).toBeInTheDocument();
    expect(await screen.findByText('由 Alice 创建')).toBeInTheDocument();
  });

  it('shows unused state when last_used_at is absent', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === '/api/v1/members') {
        return jsonResponse({ items: [] });
      }
      if (url === '/api/v1/api-keys') {
        return jsonResponse({
          items: [
            {
              id: 21,
              scopes: ['links:read'],
              status: 'active',
              created_at: '2026-11-23T08:00:00Z',
            },
          ],
        });
      }
      throw new Error(`unexpected fetch: ${url}`);
    });

    renderWithOrg(<APIKeysPage />, { orgId: 5, role: 'admin' });

    expect(await screen.findByText('#21')).toBeInTheDocument();
    expect(screen.getByText('尚未使用')).toBeInTheDocument();
  });
});
