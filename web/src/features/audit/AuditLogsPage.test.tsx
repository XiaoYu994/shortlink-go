import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { jsonResponse, renderWithOrg } from '@/test/test-utils';
import { AuditLogsPage } from './AuditLogsPage';

describe('AuditLogsPage', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('blocks member role without firing /api/v1/audit-logs request', () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch');

    renderWithOrg(<AuditLogsPage />, { orgId: 2, role: 'member' });

    expect(screen.getByText('仅所有者与管理员可查看审计日志')).toBeInTheDocument();
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('loads audit logs, links short code targets, renders diff badges, filters by action, and fetches next page', async () => {
    const user = userEvent.setup();

    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === '/api/v1/capabilities') {
        return jsonResponse({
          auth_methods: [{ id: 'oidc', kind: 'redirect', login_url: '/api/v1/auth/oidc/login' }],
          features: { analytics: true, webhooks: false, risk_scan: true, email: false },
        });
      }
      if (url === '/api/v1/audit-logs?limit=20') {
        return jsonResponse({
          items: [
            {
              id: 21,
              actor_kind: 'user',
              actor_id: 7,
              actor_email: 'alice@example.com',
              actor_name: 'Alice',
              action: 'member.role_update',
              target_type: 'member',
              target_id: '11',
              request_id: 'req-role-21',
              metadata: { previous_role: 'member', new_role: 'admin' },
              created_at: '2026-11-24T08:00:00Z',
            },
            {
              id: 20,
              actor_kind: 'system',
              action: 'link.ban',
              target_type: 'link',
              target_id: 'AbC12345',
              request_id: 'job:9',
              metadata: { reason: 'phishing', source: 'risk' },
              created_at: '2026-11-24T07:55:00Z',
            },
          ],
          next_cursor: '20',
        });
      }
      if (url === '/api/v1/audit-logs?limit=20&cursor=20') {
        return jsonResponse({
          items: [
            {
              id: 19,
              actor_kind: 'operator',
              action: 'tenant.limits_update',
              target_type: 'org',
              target_id: '5',
              metadata: { link_quota: 2000 },
              created_at: '2026-11-24T07:00:00Z',
            },
          ],
        });
      }
      if (url === '/api/v1/audit-logs?limit=20&action=link.ban') {
        return jsonResponse({
          items: [
            {
              id: 20,
              actor_kind: 'system',
              action: 'link.ban',
              target_type: 'link',
              target_id: 'AbC12345',
              request_id: 'job:9',
              metadata: { reason: 'phishing', source: 'risk' },
              created_at: '2026-11-24T07:55:00Z',
            },
          ],
        });
      }
      throw new Error(`unexpected fetch: ${url}`);
    });

    renderWithOrg(<AuditLogsPage />, { orgId: 5, role: 'owner' });

    expect(await screen.findByText('Alice')).toBeInTheDocument();
    expect(screen.getByText('成员 → 管理员')).toBeInTheDocument();
    expect(screen.getByText('安全系统')).toBeInTheDocument();

    const actionSelect = screen.getByLabelText('按操作筛选');
    await waitFor(() => {
      expect(actionSelect.querySelector('optgroup[label="Webhooks"]')).toBeNull();
    });

    const targetLink = screen.getByRole('link', { name: 'AbC12345' });
    expect(targetLink).toHaveAttribute('href', '/orgs/5/links/AbC12345');

    await user.click(screen.getByRole('button', { name: '加载更多记录' }));
    expect(await screen.findByText('平台管理员')).toBeInTheDocument();

    await user.selectOptions(screen.getByLabelText('按操作筛选'), 'link.ban');
    await waitFor(() => {
      expect(fetchSpy).toHaveBeenCalledWith(
        '/api/v1/audit-logs?limit=20&action=link.ban',
        expect.any(Object),
      );
    });
    expect(screen.queryByText('Alice')).not.toBeInTheDocument();
    expect(await screen.findByText('AbC12345')).toBeInTheDocument();
  });

  it('includes Webhooks action group when features.webhooks is true', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === '/api/v1/capabilities') {
        return jsonResponse({
          auth_methods: [{ id: 'oidc', kind: 'redirect', login_url: '/api/v1/auth/oidc/login' }],
          features: { analytics: true, webhooks: true, risk_scan: true, email: false },
        });
      }
      if (url === '/api/v1/audit-logs?limit=20') {
        return jsonResponse({ items: [] });
      }
      throw new Error(`unexpected fetch: ${url}`);
    });

    renderWithOrg(<AuditLogsPage />, { orgId: 5, role: 'owner' });

    const actionSelect = await screen.findByLabelText('按操作筛选');
    await waitFor(() => {
      expect(actionSelect.querySelector('optgroup[label="Webhooks"]')).not.toBeNull();
    });
  });
});
