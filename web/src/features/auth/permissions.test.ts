import { describe, expect, it } from 'vitest';
import {
  assignableRolesFor,
  canAssignRole,
  canManageAPIKeys,
  canManageMembers,
  canManageWebhooks,
  canReadAuditLogs,
  canWriteLinks,
} from './permissions';

describe('permissions matrix (mirrors Go identity.MapAuthorizer)', () => {
  it('allows owner, admin, member to write links, but not viewer', () => {
    expect(canWriteLinks('owner')).toBe(true);
    expect(canWriteLinks('admin')).toBe(true);
    expect(canWriteLinks('member')).toBe(true);
    expect(canWriteLinks('viewer')).toBe(false);
  });

  it('restricts member, API key, and audit management to owner and admin', () => {
    expect(canManageMembers('owner')).toBe(true);
    expect(canManageMembers('admin')).toBe(true);
    expect(canManageMembers('member')).toBe(false);
    expect(canManageMembers('viewer')).toBe(false);

    expect(canManageAPIKeys('owner')).toBe(true);
    expect(canManageAPIKeys('admin')).toBe(true);
    expect(canManageAPIKeys('member')).toBe(false);
    expect(canManageAPIKeys('viewer')).toBe(false);

    expect(canReadAuditLogs('owner')).toBe(true);
    expect(canReadAuditLogs('admin')).toBe(true);
    expect(canReadAuditLogs('member')).toBe(false);
    expect(canReadAuditLogs('viewer')).toBe(false);

    expect(canManageWebhooks('owner')).toBe(true);
    expect(canManageWebhooks('admin')).toBe(true);
    expect(canManageWebhooks('member')).toBe(false);
    expect(canManageWebhooks('viewer')).toBe(false);
  });

  it('prevents admin from assigning or modifying owner role', () => {
    expect(canAssignRole('owner', 'owner')).toBe(true);
    expect(canAssignRole('admin', 'owner')).toBe(false);
    expect(canAssignRole('admin', 'admin')).toBe(true);
    expect(canAssignRole('admin', 'member')).toBe(true);
    expect(assignableRolesFor('owner')).toContain('owner');
    expect(assignableRolesFor('admin')).not.toContain('owner');
  });
});
