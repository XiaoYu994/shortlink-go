import type { ReactElement } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { OrgContext, type OrgContextValue } from '@/app/org-context';
import { routes } from '@/app/routes';
import type { Role } from '@/features/auth/api';

interface RenderWithOrgOptions {
  role?: Role;
  orgId?: number;
  orgName?: string;
  linkUsage?: number;
  linkQuota?: number;
  route?: string;
}

export function renderWithOrg(ui: ReactElement, opts: RenderWithOrgOptions = {}) {
  const orgId = opts.orgId ?? 1;
  const role = opts.role ?? 'owner';
  const orgName = opts.orgName ?? 'Acme';
  const linkUsage = opts.linkUsage ?? 3;
  const linkQuota = opts.linkQuota ?? 1000;

  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });

  const ctxValue: OrgContextValue = {
    user: { id: 7, email: 'alice@example.com', name: 'Alice' },
    org: { id: orgId, name: orgName, role, link_usage: linkUsage, link_quota: linkQuota },
    organizations: [
      { id: orgId, name: orgName, role, link_usage: linkUsage, link_quota: linkQuota },
    ],
  };

  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[opts.route ?? routes.orgLinks(orgId)]}>
        <OrgContext.Provider value={ctxValue}>{ui}</OrgContext.Provider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

export function jsonResponse(body: unknown, status = 200): Response {
  return new Response(status === 204 ? null : JSON.stringify(body), {
    status,
    headers: status === 204 ? undefined : { 'Content-Type': 'application/json' },
  });
}
