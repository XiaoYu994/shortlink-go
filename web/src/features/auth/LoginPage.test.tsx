import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { jsonResponse } from '@/test/test-utils';
import type { AuthMethodDescriptor } from './api';
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

  function renderLoginPage(authMethods: AuthMethodDescriptor[], initialEntry = '/login') {
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === '/api/v1/me') {
        return jsonResponse({ error: 'unauthorized' }, 401);
      }
      if (url === '/api/v1/capabilities') {
        return jsonResponse({
          auth_methods: authMethods,
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
        <MemoryRouter initialEntries={[initialEntry]}>
          <LoginPage />
        </MemoryRouter>
      </QueryClientProvider>,
    );
  }

  it('renders redirect auth methods dynamically from /api/v1/capabilities', async () => {
    renderLoginPage(
      [
        { id: 'oidc', kind: 'redirect', login_url: '/api/v1/auth/oidc/login' },
        { id: 'github', kind: 'redirect', login_url: '/api/v1/auth/github/login' },
      ],
      '/login?return_to=/orgs/1/links',
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

  it('shows unsupported notice instead of empty warning for non-redirect auth methods', async () => {
    renderLoginPage([{ id: 'phone', kind: 'phone_otp' }]);

    expect(
      await screen.findByText(/服务端已启用以下登录方式，但当前控制台页面尚不支持直接交互/),
    ).toBeInTheDocument();
    expect(screen.getByText('phone')).toBeInTheDocument();
    expect(screen.queryByText(/未启用任何用户登录方式/)).not.toBeInTheDocument();
  });

  it('shows muted footnote below login buttons when mixed with unsupported auth methods', async () => {
    renderLoginPage([
      { id: 'oidc', kind: 'redirect', login_url: '/api/v1/auth/oidc/login' },
      { id: 'phone', kind: 'phone_otp' },
    ]);

    expect(await screen.findByRole('link', { name: /使用 OIDC 单点登录/ })).toBeInTheDocument();
    expect(screen.getByText(/另有服务端已启用但当前页面暂未支持的登录方式/)).toBeInTheDocument();
    expect(screen.getByText('phone')).toBeInTheDocument();
    expect(
      screen.queryByText(/服务端已启用以下登录方式，但当前控制台页面尚不支持直接交互/),
    ).not.toBeInTheDocument();
  });
});
