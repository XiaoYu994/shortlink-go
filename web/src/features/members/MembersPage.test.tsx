import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ENDPOINTS } from '@/lib/endpoints';
import { jsonResponse, renderWithOrg } from '@/test/test-utils';
import { MembersPage } from './MembersPage';

describe('MembersPage', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('enforces admin vs owner boundary: admin can update member but cannot edit or remove owner', async () => {
    const user = userEvent.setup();
    let bobRole: 'member' | 'viewer' = 'member';

    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
      const url = String(input);
      if (url === ENDPOINTS.members.detail(2) && init?.method === 'PATCH') {
        bobRole = 'viewer';
        return jsonResponse({
          user_id: 2,
          email: 'bob@example.com',
          name: 'Bob',
          role: 'viewer',
          joined_at: '2026-11-22T08:00:00Z',
        });
      }
      if (url === ENDPOINTS.members.list) {
        return jsonResponse({
          items: [
            {
              user_id: 1,
              email: 'owner@example.com',
              name: 'Org Owner',
              role: 'owner',
              joined_at: '2026-11-20T08:00:00Z',
            },
            {
              user_id: 7,
              email: 'alice@example.com',
              name: 'Alice',
              role: 'admin',
              joined_at: '2026-11-21T08:00:00Z',
            },
            {
              user_id: 2,
              email: 'bob@example.com',
              name: 'Bob',
              role: bobRole,
              joined_at: '2026-11-22T08:00:00Z',
            },
          ],
        });
      }
      if (url === ENDPOINTS.invitations.listCreate) {
        return jsonResponse({ items: [] });
      }
      throw new Error(`unexpected fetch: ${url}`);
    });

    renderWithOrg(<MembersPage />, { orgId: 4, role: 'admin' });

    expect(await screen.findByText('Org Owner')).toBeInTheDocument();
    // Admin 不能修改 Owner 的角色，所以 Owner 这一行只渲染只读徽章而非下拉框。
    expect(screen.queryByLabelText('修改 owner@example.com 的角色')).not.toBeInTheDocument();

    const bobSelect = screen.getByLabelText('修改 bob@example.com 的角色');
    const optionValues = Array.from(bobSelect.querySelectorAll('option')).map((opt) => opt.value);
    expect(optionValues).toEqual(['admin', 'member', 'viewer']);

    await user.selectOptions(bobSelect, 'viewer');

    await waitFor(() => {
      expect(fetchSpy).toHaveBeenCalledWith(
        ENDPOINTS.members.detail(2),
        expect.objectContaining({
          method: 'PATCH',
          headers: expect.objectContaining({ 'X-Org-ID': '4' }),
          body: JSON.stringify({ role: 'viewer' }),
        }),
      );
    });
  });

  it('switches to invitations tab and exposes copy-login action for pending invitation', async () => {
    const user = userEvent.setup();
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === ENDPOINTS.members.list) {
        return jsonResponse({ items: [] });
      }
      if (url === ENDPOINTS.invitations.listCreate) {
        return jsonResponse({
          items: [
            {
              id: 15,
              email: 'carol@example.com',
              role: 'member',
              status: 'pending',
              created_at: '2026-11-23T09:00:00Z',
            },
          ],
        });
      }
      throw new Error(`unexpected fetch: ${url}`);
    });

    renderWithOrg(<MembersPage />, { orgId: 4, role: 'owner' });

    await user.click(await screen.findByRole('button', { name: /邀请记录/ }));

    expect(await screen.findByText('carol@example.com')).toBeInTheDocument();
    expect(screen.getByText('待兑现')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '复制登录入口' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '撤销邀请' })).toBeInTheDocument();
  });
});
