import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { jsonResponse, renderWithOrg } from '@/test/test-utils';
import { WebhooksPage } from './WebhooksPage';

describe('WebhooksPage', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('blocks member role without firing /api/v1/webhooks request', () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      if (String(input) === '/api/v1/capabilities') {
        return jsonResponse({
          auth_methods: [{ id: 'oidc', kind: 'redirect', login_url: '/api/v1/auth/oidc/login' }],
          features: { analytics: true, webhooks: true, risk_scan: true, email: false },
        });
      }
      throw new Error(`unexpected fetch: ${String(input)}`);
    });

    renderWithOrg(<WebhooksPage />, { orgId: 2, role: 'member' });

    expect(screen.getByText('仅所有者与管理员可管理 Webhooks')).toBeInTheDocument();
    expect(fetchSpy).not.toHaveBeenCalledWith('/api/v1/webhooks', expect.anything());
  });

  it('shows disabled notice when server does not enable webhooks', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      if (String(input) === '/api/v1/capabilities') {
        return jsonResponse({
          auth_methods: [{ id: 'oidc', kind: 'redirect', login_url: '/api/v1/auth/oidc/login' }],
          features: { analytics: true, webhooks: false, risk_scan: true, email: false },
        });
      }
      throw new Error(`unexpected fetch: ${String(input)}`);
    });

    renderWithOrg(<WebhooksPage />, { orgId: 2, role: 'owner' });

    expect(await screen.findByText('Webhook 功能暂未开启')).toBeInTheDocument();
    expect(fetchSpy).not.toHaveBeenCalledWith('/api/v1/webhooks', expect.anything());
  });

  it('shows retryable error alert instead of infinite skeleton when capabilities fails', async () => {
    const user = userEvent.setup();
    let failCapabilities = true;
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === '/api/v1/capabilities') {
        if (failCapabilities) {
          return jsonResponse({ error: 'service unavailable' }, 503);
        }
        return jsonResponse({
          auth_methods: [{ id: 'oidc', kind: 'redirect', login_url: '/api/v1/auth/oidc/login' }],
          features: { analytics: true, webhooks: true, risk_scan: true, email: false },
        });
      }
      if (url === '/api/v1/webhooks') {
        return jsonResponse({ items: [] });
      }
      throw new Error(`unexpected fetch: ${url}`);
    });

    renderWithOrg(<WebhooksPage />, { orgId: 2, role: 'owner' });

    expect(await screen.findByText(/功能状态加载失败/)).toBeInTheDocument();
    expect(fetchSpy).not.toHaveBeenCalledWith('/api/v1/webhooks', expect.anything());

    failCapabilities = false;
    await user.click(screen.getByRole('button', { name: '重试' }));
    expect(await screen.findByText('还没有 Webhook 订阅')).toBeInTheDocument();
  });

  it('creates webhook, reveals secret once, triggers test ping from menu, and inspects deliveries drawer', async () => {
    const user = userEvent.setup();
    const secret = 'whsec_' + 'a'.repeat(64);
    let created = false;

    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
      const url = String(input);
      if (url === '/api/v1/capabilities') {
        return jsonResponse({
          auth_methods: [{ id: 'oidc', kind: 'redirect', login_url: '/api/v1/auth/oidc/login' }],
          features: { analytics: true, webhooks: true, risk_scan: true, email: false },
        });
      }
      if (url === '/api/v1/webhooks' && init?.method === 'POST') {
        created = true;
        return jsonResponse(
          {
            id: 12,
            url: 'https://hooks.example.com/events',
            description: 'prod-sink',
            events: ['link.created', 'link.banned'],
            secret_prefix: secret.slice(0, 12),
            enabled: true,
            consecutive_failures: 0,
            created_at: '2026-11-25T08:00:00Z',
            updated_at: '2026-11-25T08:00:00Z',
            secret,
          },
          201,
        );
      }
      if (url === '/api/v1/webhooks') {
        return jsonResponse({
          items: created
            ? [
                {
                  id: 12,
                  url: 'https://hooks.example.com/events',
                  description: 'prod-sink',
                  events: ['link.created', 'link.banned'],
                  secret_prefix: secret.slice(0, 12),
                  enabled: true,
                  consecutive_failures: 0,
                  created_at: '2026-11-25T08:00:00Z',
                  updated_at: '2026-11-25T08:00:00Z',
                },
              ]
            : [],
        });
      }
      if (url === '/api/v1/webhooks/12/test' && init?.method === 'POST') {
        return jsonResponse({ job_id: 91, delivery_id: 'dlv_91' }, 202);
      }
      if (url === '/api/v1/webhooks/12/deliveries?limit=20') {
        return jsonResponse({
          items: [
            {
              id: 501,
              webhook_id: 12,
              job_id: 91,
              delivery_id: 'dlv_91',
              event: 'webhook.ping',
              attempt: 1,
              status: 'success',
              http_status: 200,
              duration_ms: 14,
              created_at: '2026-11-25T08:01:00Z',
            },
          ],
        });
      }
      throw new Error(`unexpected fetch: ${url}`);
    });

    renderWithOrg(<WebhooksPage />, { orgId: 5, role: 'owner' });

    await screen.findByText('还没有 Webhook 订阅');
    await user.click(screen.getByRole('button', { name: /添加第一条订阅/ }));
    await user.type(
      await screen.findByLabelText('接收通知地址'),
      'https://hooks.example.com/events',
    );
    await user.type(screen.getByLabelText('备注（可选）'), 'prod-sink');
    await user.click(screen.getByRole('button', { name: '确认创建' }));

    await waitFor(() => {
      expect(fetchSpy).toHaveBeenCalledWith(
        '/api/v1/webhooks',
        expect.objectContaining({ method: 'POST' }),
      );
    });

    const secretNode = await screen.findByTestId('webhook-plaintext-secret');
    expect(secretNode).not.toHaveTextContent(secret);
    await user.click(screen.getByRole('button', { name: '显示明文密钥' }));
    expect(secretNode).toHaveTextContent(secret);
    expect(await screen.findByText('https://hooks.example.com/events')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '操作 Webhook #12' }));
    await user.click(await screen.findByRole('menuitem', { name: /发送测试请求/ }));
    await waitFor(() => {
      expect(fetchSpy).toHaveBeenCalledWith(
        '/api/v1/webhooks/12/test',
        expect.objectContaining({ method: 'POST' }),
      );
    });

    await user.click(screen.getByRole('button', { name: /投递记录/ }));
    expect(await screen.findByText('HTTP 200')).toBeInTheDocument();
    expect(screen.getByText(/dlv_91/)).toBeInTheDocument();
  });
});
