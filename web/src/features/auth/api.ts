import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiRequest } from '@/lib/api-client';
import { ENDPOINTS } from '@/lib/endpoints';

export type Role = 'owner' | 'admin' | 'member' | 'viewer';

export interface AuthProviders {
  oidc: boolean;
  github: boolean;
  webhooks?: boolean;
  public_base_url?: string;
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
  link_usage: number;
  link_quota: number;
}

export interface MeResponse {
  user: UserSummary;
  expires_at: string;
  organizations: OrgMembership[];
}

export const authKeys = {
  providers: ['auth', 'providers'] as const,
  me: ['auth', 'me'] as const,
};

export function fetchProviders(signal?: AbortSignal): Promise<AuthProviders> {
  return apiRequest<AuthProviders>(ENDPOINTS.auth.providers, {
    signal,
    skipAuthRedirect: true,
  });
}

export function fetchMe(signal?: AbortSignal): Promise<MeResponse> {
  return apiRequest<MeResponse>(ENDPOINTS.auth.me, {
    signal,
    skipAuthRedirect: true,
  });
}

export function postLogout(): Promise<void> {
  return apiRequest<void>(ENDPOINTS.auth.logout, {
    method: 'POST',
    skipAuthRedirect: true,
  });
}

export function useAuthProviders() {
  return useQuery({
    queryKey: authKeys.providers,
    queryFn: ({ signal }) => fetchProviders(signal),
    staleTime: 5 * 60_000,
  });
}

// 读取后端配置的公开短链跳转域名；未配置或加载中时返回 undefined（调用方自动回退当前 origin）。
export function usePublicBaseUrl(): string | undefined {
  const { data } = useAuthProviders();
  return data?.public_base_url;
}

// 报告后端是否启用了 Webhook 管理（配置了 WEBHOOK_SECRET_KEY）。
export function useWebhooksEnabled(): boolean {
  const { data } = useAuthProviders();
  return Boolean(data?.webhooks);
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
    onSuccess: () => {
      queryClient.clear();
    },
  });
}
