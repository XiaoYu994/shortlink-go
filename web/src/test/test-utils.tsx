import type { ReactElement } from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { OrgContext, type OrgContextValue } from '@/app/org-context';
import type { Role } from '@/features/auth/api';

interface RenderWithOrgOptions {
  role?: Role;
  orgId?: number;
  orgName?: string;
  route?: string;
}

/*
  测试辅助封装（类比 Go 测试里的 newTestServer）：
  为被测页面挂上干净的 QueryClient（关闭 retry 避免测试超时）、MemoryRouter 与 OrgContext。
*/
export function renderWithOrg(ui: ReactElement, opts: RenderWithOrgOptions = {}) {
  const orgId = opts.orgId ?? 1;
  const role = opts.role ?? 'owner';
  const orgName = opts.orgName ?? 'Acme';

  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });

  const ctxValue: OrgContextValue = {
    user: { id: 7, email: 'alice@example.com', name: 'Alice' },
    org: { id: orgId, name: orgName, role },
    organizations: [{ id: orgId, name: orgName, role }],
  };

  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[opts.route ?? `/orgs/${orgId}/links`]}>
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
