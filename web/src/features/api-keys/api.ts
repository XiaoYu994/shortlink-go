import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiRequest } from '@/lib/api-client';

export type APIKeyStatus = 'active' | 'revoked' | 'expired';
export type APIKeyScope = 'links:read' | 'links:write' | 'stats:read';

export interface APIKeyInfo {
  id: number;
  scopes: APIKeyScope[];
  status: APIKeyStatus;
  created_by?: number;
  created_at: string;
  expires_at?: string;
  revoked_at?: string;
}

export interface IssuedAPIKey extends APIKeyInfo {
  api_key: string;
}

export const ALL_API_KEY_SCOPES: ReadonlyArray<{
  value: APIKeyScope;
  title: string;
  description: string;
}> = [
  { value: 'links:read', title: 'links:read', description: '查询短链列表与单条详情' },
  { value: 'links:write', title: 'links:write', description: '创建、修改、停用与删除短链' },
  { value: 'stats:read', title: 'stats:read', description: '查询短链点击与访客统计' },
];

export const apiKeyKeys = {
  list: (orgId: number) => ['orgs', orgId, 'api-keys'] as const,
};

export function useAPIKeys(orgId: number, enabled: boolean) {
  return useQuery({
    queryKey: apiKeyKeys.list(orgId),
    queryFn: async ({ signal }) => {
      const res = await apiRequest<{ items: APIKeyInfo[] }>('/api/v1/api-keys', { orgId, signal });
      return res.items;
    },
    enabled,
  });
}

export function useIssueAPIKey(orgId: number) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (payload: { scopes: APIKeyScope[]; expires_at?: string }) =>
      apiRequest<IssuedAPIKey>('/api/v1/api-keys', {
        method: 'POST',
        orgId,
        body: payload,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: apiKeyKeys.list(orgId) });
    },
  });
}

export function useRevokeAPIKey(orgId: number) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) =>
      apiRequest<void>(`/api/v1/api-keys/${id}`, {
        method: 'DELETE',
        orgId,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: apiKeyKeys.list(orgId) });
    },
  });
}
