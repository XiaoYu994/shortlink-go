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

  it('enforces admin boundary and requires confirmation before self-downgrade without leaking PATCH on cancel', async () => {
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
      if (url === ENDPOINTS.members.detail(7) && init?.method === 'PATCH') {
        return jsonResponse({
          user_id: 7,
          email: 'alice@example.com',
          name: 'Alice',
          role: 'viewer',
          joined_at: '2026-11-21T08:00:00Z',
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

    // 管理员调整自己角色时先弹出确认框，取消后不发 PATCH 请求。
    const aliceSelect = screen.getByLabelText('修改 alice@example.com 的角色');
    await user.selectOptions(aliceSelect, 'viewer');
    expect(await screen.findByText(/确认将自己的角色改为/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '取消' }));

    expect(
      fetchSpy.mock.calls.some(
        ([u, reqInit]) => u === ENDPOINTS.members.detail(7) && reqInit?.method === 'PATCH',
      ),
    ).toBe(false);
  });

  it('switches to invitations tab with pending badge count and exposes copy-login action', async () => {
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
            {
              id: 16,
              email: 'done@example.com',
              role: 'viewer',
              status: 'accepted',
              created_at: '2026-11-22T09:00:00Z',
            },
          ],
        });
      }
      throw new Error(`unexpected fetch: ${url}`);
    });

    renderWithOrg(<MembersPage />, { orgId: 4, role: 'owner' });

    expect(await screen.findByText('1 待加入')).toBeInTheDocument();
    await user.click(screen.getByRole('tab', { name: /邀请记录/ }));

    expect(await screen.findByText('carol@example.com')).toBeInTheDocument();
    expect(screen.getByText('待加入')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '复制登录入口' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '撤销邀请' })).toBeInTheDocument();
  });

  it('disables role selector for sole owner while allowing changes when multiple owners exist', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === ENDPOINTS.members.list) {
        return jsonResponse({
          items: [
            {
              user_id: 7,
              email: 'alice@example.com',
              name: 'Alice',
              role: 'owner',
              joined_at: '2026-11-20T08:00:00Z',
            },
          ],
        });
      }
      if (url === ENDPOINTS.invitations.listCreate) {
        return jsonResponse({ items: [] });
      }
      throw new Error(`unexpected fetch: ${url}`);
    });

    renderWithOrg(<MembersPage />, { orgId: 4, role: 'owner' });

    const ownerSelect = await screen.findByLabelText('修改 alice@example.com 的角色');
    expect(ownerSelect).toBeDisabled();
    expect(fetchSpy.mock.calls.some(([, reqInit]) => reqInit?.method === 'PATCH')).toBe(false);
  });

  it.each([
    {
      label: 'automatic email notice when features.email is true',
      emailEnabled: true,
      invitee: 'dave@example.com',
      wantShown: /系统将自动向/,
      wantHidden: /当前未开启自动邀请邮件/,
    },
    {
      label: 'manual copy notice when features.email is false',
      emailEnabled: false,
      invitee: 'erin@example.com',
      wantShown: /当前未开启自动邀请邮件/,
      wantHidden: /系统将自动向/,
    },
  ])(
    'shows $label after creating invitation',
    async ({ emailEnabled, invitee, wantShown, wantHidden }) => {
      const user = userEvent.setup();
      vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
        const url = String(input);
        if (url === ENDPOINTS.capabilities) {
          return jsonResponse({
            auth_methods: [{ id: 'oidc', kind: 'redirect', login_url: '/api/v1/auth/oidc/login' }],
            features: { analytics: true, webhooks: true, risk_scan: true, email: emailEnabled },
          });
        }
        if (url === ENDPOINTS.members.list) {
          return jsonResponse({ items: [] });
        }
        if (url === ENDPOINTS.invitations.listCreate && init?.method === 'POST') {
          return jsonResponse(
            {
              id: 21,
              email: invitee,
              role: 'member',
              status: 'pending',
              created_at: '2026-11-26T09:00:00Z',
            },
            201,
          );
        }
        if (url === ENDPOINTS.invitations.listCreate) {
          return jsonResponse({ items: [] });
        }
        throw new Error(`unexpected fetch: ${url}`);
      });

      renderWithOrg(<MembersPage />, { orgId: 4, role: 'owner' });

      await user.click(await screen.findByRole('button', { name: '邀请成员' }));
      await user.type(await screen.findByLabelText('受邀人邮箱'), invitee);
      await user.click(screen.getByRole('button', { name: '发起邀请' }));

      expect(await screen.findByText('邀请已登记')).toBeInTheDocument();
      expect(screen.getByText(wantShown)).toBeInTheDocument();
      expect(screen.queryByText(wantHidden)).not.toBeInTheDocument();
    },
  );
});
