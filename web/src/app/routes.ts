/*
  前端路由表与类型安全的路径构造函数（Single Source of Truth）。
  路由注册（router.tsx）和所有跳转/链接（NavLink / navigate / Link）共用这一处定义，
  避免路径模板散落在多个页面里或用 pathname.split('/') 魔法下标解析路由。
*/

export const ROUTE_PATTERNS = {
  root: '/',
  login: '/login',
  onboarding: '/onboarding',
  orgRoot: '/orgs/:orgId',
  orgLinks: '/orgs/:orgId/links',
  orgLinkStats: '/orgs/:orgId/links/:code',
  orgMembers: '/orgs/:orgId/members',
  orgAPIKeys: '/orgs/:orgId/api-keys',
  orgWebhooks: '/orgs/:orgId/webhooks',
  orgAuditLogs: '/orgs/:orgId/audit-logs',
} as const;

export type OrgSection = 'links' | 'members' | 'api-keys' | 'webhooks' | 'audit-logs';

export const routes = {
  root: () => ROUTE_PATTERNS.root,
  login: (returnTo?: string) => {
    if (!returnTo || returnTo === '/' || returnTo.startsWith(ROUTE_PATTERNS.login)) {
      return ROUTE_PATTERNS.login;
    }
    return `${ROUTE_PATTERNS.login}?return_to=${encodeURIComponent(returnTo)}`;
  },
  onboarding: () => ROUTE_PATTERNS.onboarding,
  orgSection: (orgId: number, section: OrgSection = 'links') => `/orgs/${orgId}/${section}`,
  orgLinks: (orgId: number) => `/orgs/${orgId}/links`,
  linkStats: (orgId: number, code: string) => `/orgs/${orgId}/links/${encodeURIComponent(code)}`,
  orgMembers: (orgId: number) => `/orgs/${orgId}/members`,
  orgAPIKeys: (orgId: number) => `/orgs/${orgId}/api-keys`,
  orgWebhooks: (orgId: number) => `/orgs/${orgId}/webhooks`,
  orgAuditLogs: (orgId: number) => `/orgs/${orgId}/audit-logs`,
} as const;
