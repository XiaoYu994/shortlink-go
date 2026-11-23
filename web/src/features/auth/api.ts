import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiRequest } from '@/lib/api-client';

export type Role = 'owner' | 'admin' | 'member' | 'viewer';

export interface AuthProviders {
  oidc: boolean;
  github: boolean;
}

export interface UserSummary {
  id: number;
  email: string;
  name: string;
}

export interface OrgMembership {
  id: number;
  name: string;
  role: Role;
}

export interface MeResponse {
  user: UserSummary;
  expires_at: string;
  organizations: OrgMembership[];
}

// Query Key 工厂：类似后端 Redis key 前缀，统一管理缓存失效范围。
export const authKeys = {
  providers: ['auth', 'providers'] as const,
  me: ['auth', 'me'] as const,
};

export function fetchProviders(signal?: AbortSignal): Promise<AuthProviders> {
  return apiRequest<AuthProviders>('/api/v1/auth/providers', { signal });
}

export function fetchMe(signal?: AbortSignal): Promise<MeResponse> {
  return apiRequest<MeResponse>('/api/v1/me', { signal });
}

export function postLogout(): Promise<void> {
  return apiRequest<void>('/api/v1/auth/logout', { method: 'POST' });
}

export function useAuthProviders() {
  return useQuery({
    queryKey: authKeys.providers,
    queryFn: ({ signal }) => fetchProviders(signal),
    staleTime: 5 * 60_000,
  });
}

export function useMe() {
  return useQuery({
    queryKey: authKeys.me,
    queryFn: ({ signal }) => fetchMe(signal),
    staleTime: 60_000,
  });
}

export function useLogout() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: postLogout,
    onSettled: () => {
      // 登出后清空全部服务端缓存，避免切换账号时短暂看到上一个用户的组织或短链。
      queryClient.clear();
    },
  });
}
