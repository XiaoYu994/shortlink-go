import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { jsonResponse } from '@/test/test-utils';
import { LoginPage } from './LoginPage';
import { sanitizeReturnTo } from './return-to';

describe('sanitizeReturnTo', () => {
  it('accepts safe relative console routes', () => {
    expect(sanitizeReturnTo('/orgs/1/links?sort=clicks')).toBe('/orgs/1/links?sort=clicks');
    expect(sanitizeReturnTo('/onboarding')).toBe('/onboarding');
    expect(sanitizeReturnTo('/')).toBe('/');
  });

  it('rejects open redirects, traversal to reserved prefixes, backslashes, and control chars', () => {
    expect(sanitizeReturnTo(null)).toBe('/');
    expect(sanitizeReturnTo('https://evil.example')).toBe('/');
    expect(sanitizeReturnTo('//evil.example')).toBe('/');
    expect(sanitizeReturnTo('/\\evil.example')).toBe('/');
    expect(sanitizeReturnTo('/orgs\n/1')).toBe('/');
    expect(sanitizeReturnTo('/x/../api/v1/me')).toBe('/');
    expect(sanitizeReturnTo('/x/../r/AbC123xy')).toBe('/');
    expect(sanitizeReturnTo('/api/v1/auth/logout')).toBe('/');
    expect(sanitizeReturnTo('/r/AbC123xy')).toBe('/');
  });
});

describe('LoginPage', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('renders redirect auth methods dynamically from /api/v1/capabilities', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === '/api/v1/me') {
        return jsonResponse({ error: 'unauthorized' }, 401);
      }
      if (url === '/api/v1/capabilities') {
        return jsonResponse({
          auth_methods: [
            { id: 'oidc', kind: 'redirect', login_url: '/api/v1/auth/oidc/login' },
            { id: 'github', kind: 'redirect', login_url: '/api/v1/auth/github/login' },
          ],
          features: { analytics: true, webhooks: false, risk_scan: true, email: false },
        });
      }
      throw new Error(`unexpected fetch: ${url}`);
    });

    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/login?return_to=/orgs/1/links']}>
          <LoginPage />
        </MemoryRouter>
      </QueryClientProvider>,
    );

    const oidcLink = await screen.findByRole('link', { name: /使用 OIDC 单点登录/ });
    expect(oidcLink).toHaveAttribute(
      'href',
      '/api/v1/auth/oidc/login?return_to=%2Forgs%2F1%2Flinks',
    );
    const ghLink = screen.getByRole('link', { name: /使用 GitHub 账号登录/ });
    expect(ghLink).toHaveAttribute(
      'href',
      '/api/v1/auth/github/login?return_to=%2Forgs%2F1%2Flinks',
    );
  });
});
