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

    expect(screen.getByText('仅 Owner 与 Admin 可查看审计日志')).toBeInTheDocument();
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('loads audit logs, filters by action, and fetches next page via cursor', async () => {
    const user = userEvent.setup();

    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === '/api/v1/audit-logs?limit=20') {
        return jsonResponse({
          items: [
            {
              id: 21,
              actor_kind: 'user',
              actor_id: 7,
              actor_email: 'alice@example.com',
              actor_name: 'Alice',
              action: 'apikey.issue',
              target_type: 'api_key',
              target_id: '11',
              request_id: 'req-issue-21',
              metadata: { scopes: ['links:read'] },
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
    expect(screen.getByText('系统任务')).toBeInTheDocument();
    expect(screen.getByText('AbC12345')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '加载更多记录' }));
    expect(await screen.findByText('运维 CLI')).toBeInTheDocument();

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
});
