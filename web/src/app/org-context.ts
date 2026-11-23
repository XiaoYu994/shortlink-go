import { createContext, useContext } from 'react';
import type { OrgMembership, UserSummary } from '@/features/auth/api';

/*
  OrgContext 保存「当前已认证用户 + 当前路由对应的组织」。
  类比 Go 里挂载到 request context 的 identity.Principal：
  凡是挂在 /orgs/:orgId/* 下面的业务页面，都直接调用 useCurrentOrg() 拿租户上下文，
  不必从 App 一层层 prop drilling 传下去。
*/
export interface OrgContextValue {
  user: UserSummary;
  org: OrgMembership;
  organizations: OrgMembership[];
}

export const OrgContext = createContext<OrgContextValue | null>(null);

export function useCurrentOrg(): OrgContextValue {
  const ctx = useContext(OrgContext);
  if (!ctx) {
    throw new Error('useCurrentOrg must be used inside OrgRouteGuard');
  }
  return ctx;
}
