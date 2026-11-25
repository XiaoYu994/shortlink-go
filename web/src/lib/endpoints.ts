/*
  后端 HTTP API 端点集中注册表（Single Source of Truth）。
  - 统一收口版本前缀 /api/v1，未来按 ADR 0001 演进到 /api/v2 或调整单个端点时只改这一处；
  - 页面组件与业务模块一律通过 ENDPOINTS 引用路径，禁止直接手写 "/api/..." 字面量。
*/

export const API_V1_PREFIX = '/api/v1' as const;

const v1 = (path: string) => `${API_V1_PREFIX}${path}`;

export const ENDPOINTS = {
  auth: {
    providers: v1('/auth/providers'),
    me: v1('/me'),
    logout: v1('/auth/logout'),
    oidcLogin: (returnTo: string) =>
      `${v1('/auth/login')}?return_to=${encodeURIComponent(returnTo)}`,
    githubLogin: (returnTo: string) =>
      `${v1('/auth/github/login')}?return_to=${encodeURIComponent(returnTo)}`,
  },
  orgs: {
    listCreate: v1('/orgs'),
  },
  links: {
    listCreate: v1('/links'),
    detail: (code: string) => v1(`/links/${encodeURIComponent(code)}`),
    stats: (code: string) => v1(`/links/${encodeURIComponent(code)}/stats`),
  },
  members: {
    list: v1('/members'),
    detail: (userId: number) => v1(`/members/${userId}`),
  },
  invitations: {
    listCreate: v1('/invitations'),
    detail: (id: number) => v1(`/invitations/${id}`),
  },
  apiKeys: {
    listCreate: v1('/api-keys'),
    detail: (id: number) => v1(`/api-keys/${id}`),
  },
  webhooks: {
    listCreate: v1('/webhooks'),
    detail: (id: number) => v1(`/webhooks/${id}`),
    rotateSecret: (id: number) => v1(`/webhooks/${id}/rotate-secret`),
    test: (id: number) => v1(`/webhooks/${id}/test`),
    deliveries: (id: number) => v1(`/webhooks/${id}/deliveries`),
  },
  auditLogs: {
    list: v1('/audit-logs'),
  },
} as const;
