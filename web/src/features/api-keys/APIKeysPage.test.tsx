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

  it('issues an API key and reveals plaintext secret with curl snippet', async () => {
    const user = userEvent.setup();
    const plaintext = 'slk_' + 'a'.repeat(64);
    const fetchSpy = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValueOnce(jsonResponse({ items: [] }))
      .mockResolvedValueOnce(
        jsonResponse(
          {
            id: 11,
            scopes: ['links:read', 'links:write', 'stats:read'],
            status: 'active',
            created_by: 7,
            created_at: '2026-11-23T08:00:00Z',
            api_key: plaintext,
          },
          201,
        ),
      )
      .mockResolvedValueOnce(
        jsonResponse({
          items: [
            {
              id: 11,
              scopes: ['links:read', 'links:write', 'stats:read'],
              status: 'active',
              created_by: 7,
              created_at: '2026-11-23T08:00:00Z',
            },
          ],
        }),
      );

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
  });
});
