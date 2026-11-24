import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { apiRequest } from '@/lib/api-client';
import { ENDPOINTS } from '@/lib/endpoints';
import { jsonResponse } from '@/test/test-utils';
import { OrgRouteGuard, RootRedirect } from './AuthGuard';
import { ROUTE_PATTERNS } from './routes';

function LocationProbe({ testId }: { testId: string }) {
  const location = useLocation();
  return <div data-testid={testId}>{location.pathname + location.search}</div>;
}

function ExpireSessionTrigger() {
  return (
    <button
      type="button"
      onClick={() => {
        void apiRequest(ENDPOINTS.members.list, { orgId: 3 }).catch(() => {});
      }}
    >
      触发接口 401
    </button>
  );
}

function renderWithGuard(initialEntry: string) {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });

  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[initialEntry]}>
        <Routes>
          <Route path={ROUTE_PATTERNS.root} element={<RootRedirect />} />
          <Route path={ROUTE_PATTERNS.login} element={<LocationProbe testId="login-page" />} />
          <Route
            path={ROUTE_PATTERNS.onboarding}
            element={<div data-testid="onboarding-page">onboarding</div>}
          />
          <Route path={ROUTE_PATTERNS.orgRoot} element={<OrgRouteGuard />}>
            <Route path="links" element={<div data-testid="org-links-page">links</div>} />
            <Route path="members" element={<ExpireSessionTrigger />} />
          </Route>
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('AuthGuard', () => {
  afterEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it('redirects unauthenticated org route to /login with encoded return_to', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse({ error: 'unauthorized' }, 401));

    renderWithGuard('/orgs/2/members?tab=invitations');

    expect(await screen.findByTestId('login-page')).toHaveTextContent(
      '/login?return_to=%2Forgs%2F2%2Fmembers%3Ftab%3Dinvitations',
    );
  });

  it('redirects authenticated user without organizations to /onboarding', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      jsonResponse({
        user: { id: 1, email: 'new@example.com', name: 'Newbie' },
        expires_at: '2026-12-01T00:00:00Z',
        organizations: [],
      }),
    );

    renderWithGuard('/orgs/1/links');

    expect(await screen.findByTestId('onboarding-page')).toBeInTheDocument();
  });

  it('falls back to first organization when :orgId is not in user memberships', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      jsonResponse({
        user: { id: 7, email: 'alice@example.com', name: 'Alice' },
        expires_at: '2026-12-01T00:00:00Z',
        organizations: [
          { id: 3, name: 'Beta Org', role: 'owner', link_usage: 5, link_quota: 1000 },
        ],
      }),
    );

    renderWithGuard('/orgs/999/members');

    expect(await screen.findByTestId('org-links-page')).toBeInTheDocument();
    expect(screen.getAllByText('Beta Org').length).toBeGreaterThan(0);
  });

  it('redirects to /login preserving return_to when a mid-session API call returns 401', async () => {
    const user = userEvent.setup();
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === ENDPOINTS.auth.me) {
        return jsonResponse({
          user: { id: 7, email: 'alice@example.com', name: 'Alice' },
          expires_at: '2026-12-01T00:00:00Z',
          organizations: [
            { id: 3, name: 'Beta Org', role: 'owner', link_usage: 5, link_quota: 1000 },
          ],
        });
      }
      if (url === ENDPOINTS.members.list) {
        return jsonResponse({ error: 'unauthorized' }, 401);
      }
      throw new Error(`unexpected fetch: ${url}`);
    });

    renderWithGuard('/orgs/3/members');

    await user.click(await screen.findByRole('button', { name: '触发接口 401' }));

    expect(await screen.findByTestId('login-page')).toHaveTextContent(
      '/login?return_to=%2Forgs%2F3%2Fmembers',
    );
  });

  it('keeps rendering workspace when background /me refetch fails with cached data', async () => {
    let meCalls = 0;
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
      const url = String(input);
      if (url === ENDPOINTS.auth.me) {
        meCalls += 1;
        if (meCalls === 1) {
          return jsonResponse({
            user: { id: 7, email: 'alice@example.com', name: 'Alice' },
            expires_at: '2026-12-01T00:00:00Z',
            organizations: [
              { id: 3, name: 'Beta Org', role: 'owner', link_usage: 5, link_quota: 1000 },
            ],
          });
        }
        return jsonResponse({ error: 'internal error' }, 500);
      }
      throw new Error(`unexpected fetch: ${url}`);
    });

    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
    });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={['/orgs/3/links']}>
          <Routes>
            <Route path={ROUTE_PATTERNS.orgRoot} element={<OrgRouteGuard />}>
              <Route path="links" element={<div data-testid="org-links-page">links</div>} />
            </Route>
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(await screen.findByTestId('org-links-page')).toBeInTheDocument();

    await queryClient.invalidateQueries({ queryKey: ['auth', 'me'] });
    expect(await screen.findByTestId('org-links-page')).toBeInTheDocument();
    expect(screen.queryByText(/加载会话失败/)).not.toBeInTheDocument();
  });
});
