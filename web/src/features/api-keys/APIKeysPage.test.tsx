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

    expect(screen.getByText('仅 Owner 与 Admin 可管理 API Key')).toBeInTheDocument();
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('issues an API key, reveals curl snippet, and resolves creator member name', async () => {
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
            scopes: ['links:read', 'links:write', 'stats:read'],
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
                  scopes: ['links:read', 'links:write', 'stats:read'],
                  status: 'active',
                  created_by: 7,
                  created_at: '2026-11-23T08:00:00Z',
                },
              ]
            : [],
        });
      }
      throw new Error(`unexpected fetch: ${url}`);
    });

    renderWithOrg(<APIKeysPage />, { orgId: 5, role: 'owner' });

    await screen.findByText('还没有组织 API Key');
    await user.click(screen.getByRole('button', { name: /签发第一把 API Key/ }));
    await user.click(await screen.findByRole('button', { name: '确认签发' }));

    await waitFor(() => {
      expect(fetchSpy).toHaveBeenCalledWith(
        '/api/v1/api-keys',
        expect.objectContaining({ method: 'POST' }),
      );
    });

    expect(await screen.findByTestId('issued-api-key-secret')).toHaveTextContent(plaintext);
    expect(screen.getByTestId('issued-api-key-curl')).toHaveTextContent(
      `Authorization: Bearer ${plaintext}`,
    );
    expect(await screen.findByText('#11')).toBeInTheDocument();
    expect(await screen.findByText('由 Alice 签发')).toBeInTheDocument();
  });
});
