import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiRequest } from '@/lib/api-client';
import { ENDPOINTS } from '@/lib/endpoints';

export type Role = 'owner' | 'admin' | 'member' | 'viewer';

export type AuthFlowKind = 'redirect' | 'phone_otp' | (string & {});

export interface AuthMethodDescriptor {
  id: string;
  kind: AuthFlowKind;
  login_url?: string;
}

export interface FeatureCapabilities {
  analytics: boolean;
  webhooks: boolean;
  risk_scan: boolean;
  email: boolean;
}

export interface Capabilities {
  auth_methods: AuthMethodDescriptor[];
  features: FeatureCapabilities;
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
  capabilities: ['capabilities'] as const,
  me: ['auth', 'me'] as const,
};

export function fetchCapabilities(signal?: AbortSignal): Promise<Capabilities> {
  return apiRequest<Capabilities>(ENDPOINTS.capabilities, {
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

export function useCapabilities() {
  return useQuery({
    queryKey: authKeys.capabilities,
    queryFn: ({ signal }) => fetchCapabilities(signal),
    staleTime: 5 * 60_000,
  });
}

// 读取后端配置的公开短链跳转域名；未配置或加载中时返回 undefined（调用方自动回退当前 origin）。
export function usePublicBaseUrl(): string | undefined {
  const { data } = useCapabilities();
  return data?.public_base_url;
}

// 报告后端是否启用了 Webhook 管理（配置了 WEBHOOK_SECRET_KEY）。
export function useWebhooksEnabled(): boolean {
  const { data } = useCapabilities();
  return Boolean(data?.features.webhooks);
}

// 报告后端是否启用了点击统计（FEATURE_ANALYTICS，默认 true）。
export function useAnalyticsEnabled(): boolean {
  const { data } = useCapabilities();
  return data ? data.features.analytics : true;
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
