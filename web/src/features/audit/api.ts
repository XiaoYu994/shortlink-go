import { useInfiniteQuery } from '@tanstack/react-query';
import { apiRequest } from '@/lib/api-client';
import { DEFAULT_AUDIT_LOGS_PAGE_SIZE } from '@/lib/constants';
import { ENDPOINTS } from '@/lib/endpoints';

export type AuditActorKind = 'user' | 'api_key' | 'operator' | 'system';

export type AuditAction =
  | 'org.create'
  | 'tenant.limits_update'
  | 'link.create'
  | 'link.update'
  | 'link.delete'
  | 'link.ban'
  | 'link.unban'
  | 'member.role_update'
  | 'member.remove'
  | 'invitation.create'
  | 'invitation.revoke'
  | 'invitation.redeem'
  | 'apikey.issue'
  | 'apikey.revoke';

export type AuditTargetType = 'org' | 'link' | 'member' | 'invitation' | 'api_key';

export interface AuditEvent {
  id: number;
  actor_kind: AuditActorKind;
  actor_id?: number;
  actor_email?: string;
  actor_name?: string;
  action: AuditAction;
  target_type: AuditTargetType;
  target_id: string;
  request_id?: string;
  metadata: Record<string, unknown>;
  created_at: string;
}

export interface ListAuditLogsResponse {
  items: AuditEvent[];
  next_cursor?: string;
}

export const auditKeys = {
  all: (orgId: number) => ['orgs', orgId, 'audit-logs'] as const,
  list: (orgId: number, action?: AuditAction | '') =>
    ['orgs', orgId, 'audit-logs', 'list', action || 'all'] as const,
};

export function fetchAuditLogs(
  orgId: number,
  params: { limit?: number; action?: AuditAction | ''; cursor?: string },
  signal?: AbortSignal,
): Promise<ListAuditLogsResponse> {
  const q = new URLSearchParams();
  if (params.limit) q.set('limit', String(params.limit));
  if (params.action) q.set('action', params.action);
  if (params.cursor) q.set('cursor', params.cursor);
  const suffix = q.toString() ? `?${q.toString()}` : '';
  return apiRequest<ListAuditLogsResponse>(`${ENDPOINTS.auditLogs.list}${suffix}`, {
    orgId,
    signal,
  });
}

export function useInfiniteAuditLogs(orgId: number, action: AuditAction | '', enabled = true) {
  return useInfiniteQuery({
    queryKey: auditKeys.list(orgId, action),
    initialPageParam: undefined as string | undefined,
    enabled,
    // 审计页没有订阅其他模块的变更缓存，切回页面时总是刷新最新事件，保持领域单向隔离。
    staleTime: 0,
    queryFn: ({ pageParam, signal }) =>
      fetchAuditLogs(
        orgId,
        { limit: DEFAULT_AUDIT_LOGS_PAGE_SIZE, action, cursor: pageParam },
        signal,
      ),
    getNextPageParam: (lastPage) => lastPage.next_cursor,
  });
}
