import { screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { jsonResponse, renderWithOrg } from '@/test/test-utils';
import { ConsoleLayout } from './ConsoleLayout';

describe('ConsoleLayout', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('hides Webhooks navigation item when features.webhooks is false', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      if (String(input) === '/api/v1/capabilities') {
        return jsonResponse({
          auth_methods: [{ id: 'oidc', kind: 'redirect', login_url: '/api/v1/auth/oidc/login' }],
          features: { analytics: true, webhooks: false, risk_scan: true, email: false },
        });
      }
      throw new Error(`unexpected fetch: ${String(input)}`);
    });

    renderWithOrg(<ConsoleLayout />, { orgId: 4, role: 'owner' });

    expect(screen.getByRole('link', { name: '短链管理' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '成员与邀请' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'API Keys' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '审计日志' })).toBeInTheDocument();

    await waitFor(() => {
      expect(screen.queryByRole('link', { name: 'Webhooks' })).not.toBeInTheDocument();
    });
  });

  it('renders Webhooks navigation item when features.webhooks is true', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      if (String(input) === '/api/v1/capabilities') {
        return jsonResponse({
          auth_methods: [{ id: 'oidc', kind: 'redirect', login_url: '/api/v1/auth/oidc/login' }],
          features: { analytics: true, webhooks: true, risk_scan: true, email: false },
        });
      }
      throw new Error(`unexpected fetch: ${String(input)}`);
    });

    renderWithOrg(<ConsoleLayout />, { orgId: 4, role: 'owner' });

    const webhooksLink = await screen.findByRole('link', { name: 'Webhooks' });
    expect(webhooksLink).toHaveAttribute('href', '/orgs/4/webhooks');
  });
});
