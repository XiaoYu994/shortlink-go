import type { Role } from './api';

/*
  前端权限辅助只负责界面体验（隐藏无权按钮、提示原因），
  真正的安全边界始终在 Go 后端的 Authorizer 与存储层校验（ADR 0018 / ADR 0021）。
*/

export const ALL_ROLES: readonly Role[] = ['owner', 'admin', 'member', 'viewer'];
const ADMIN_ASSIGNABLE_ROLES: readonly Role[] = ['admin', 'member', 'viewer'];

export function canWriteLinks(role: Role): boolean {
  return role === 'owner' || role === 'admin' || role === 'member';
}

export function canManageMembers(role: Role): boolean {
  return role === 'owner' || role === 'admin';
}

export function canAssignRole(actorRole: Role, targetRole: Role): boolean {
  if (actorRole === 'owner') return true;
  if (actorRole === 'admin') return targetRole !== 'owner';
  return false;
}

export function assignableRolesFor(actorRole: Role): readonly Role[] {
  return actorRole === 'owner' ? ALL_ROLES : ADMIN_ASSIGNABLE_ROLES;
}

export function canManageAPIKeys(role: Role): boolean {
  return role === 'owner' || role === 'admin';
}

export function canReadAuditLogs(role: Role): boolean {
  return role === 'owner' || role === 'admin';
}

export function canManageWebhooks(role: Role): boolean {
  return role === 'owner' || role === 'admin';
}

export const ROLE_LABELS: Record<Role, string> = {
  owner: '所有者',
  admin: '管理员',
  member: '成员',
  viewer: '只读成员',
};
