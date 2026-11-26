import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router';
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
          <Routes>
            <Route path="/login" element={<LoginPage />} />
            <Route path="/orgs/1/links" element={<div>links destination</div>} />
          </Routes>
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

  it('completes phone OTP send-code and verify flow from capabilities URLs', async () => {
    const user = userEvent.setup();
    let authed = false;
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
      const url = String(input);
      if (url === '/api/v1/me') {
        if (authed) {
          return jsonResponse({
            user: { id: 9, email: '', name: '+86****8000' },
            expires_at: '2026-12-01T00:00:00Z',
            organizations: [],
          });
        }
        return jsonResponse({ error: 'unauthorized' }, 401);
      }
      if (url === '/api/v1/capabilities') {
        return jsonResponse({
          auth_methods: [
            {
              id: 'phone',
              kind: 'phone_otp',
              send_code_url: '/api/v1/auth/phone/send-code',
              verify_code_url: '/api/v1/auth/phone/verify',
            },
          ],
          features: { analytics: true, webhooks: false, risk_scan: true, email: false },
        });
      }
      if (url === '/api/v1/auth/phone/send-code' && init?.method === 'POST') {
        return jsonResponse({ expires_in: 300, cooldown_seconds: 60 });
      }
      if (url === '/api/v1/auth/phone/verify' && init?.method === 'POST') {
        authed = true;
        return jsonResponse({
          user: { id: 9, email: '', name: '+86****8000' },
          expires_at: '2026-12-01T00:00:00Z',
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
          <Routes>
            <Route path="/login" element={<LoginPage />} />
            <Route path="/orgs/1/links" element={<div>links destination</div>} />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    const phoneInput = await screen.findByLabelText(/手机号/);
    await user.type(phoneInput, '+8613800138000');
    await user.click(screen.getByRole('button', { name: '获取验证码' }));

    expect(await screen.findByRole('button', { name: /60s 后重试/ })).toBeDisabled();
    await user.type(screen.getByLabelText('短信验证码'), '123456');
    await user.click(screen.getByRole('button', { name: /使用手机号登录/ }));

    expect(await screen.findByText('links destination')).toBeInTheDocument();
    await waitFor(() => {
      expect(fetchSpy).toHaveBeenCalledWith(
        '/api/v1/auth/phone/verify',
        expect.objectContaining({
          method: 'POST',
          body: JSON.stringify({ phone: '+8613800138000', code: '123456' }),
        }),
      );
    });
  });

  it('shows unsupported notice instead of empty warning for unknown auth methods', async () => {
    renderLoginPage([{ id: 'passkey', kind: 'webauthn' }]);

    expect(
      await screen.findByText(/服务端已启用以下登录方式，但当前控制台页面尚不支持直接交互/),
    ).toBeInTheDocument();
    expect(screen.getByText('passkey')).toBeInTheDocument();
    expect(screen.queryByText(/未启用任何用户登录方式/)).not.toBeInTheDocument();
  });

  it('shows muted footnote below login buttons when mixed with unknown auth methods', async () => {
    renderLoginPage([
      { id: 'oidc', kind: 'redirect', login_url: '/api/v1/auth/oidc/login' },
      { id: 'passkey', kind: 'webauthn' },
    ]);

    expect(await screen.findByRole('link', { name: /使用 OIDC 单点登录/ })).toBeInTheDocument();
    expect(screen.getByText(/另有服务端已启用但当前页面暂未支持的登录方式/)).toBeInTheDocument();
    expect(screen.getByText('passkey')).toBeInTheDocument();
    expect(
      screen.queryByText(/服务端已启用以下登录方式，但当前控制台页面尚不支持直接交互/),
    ).not.toBeInTheDocument();
  });
});
