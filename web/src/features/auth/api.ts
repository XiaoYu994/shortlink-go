import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiRequest } from '@/lib/api-client';
import { ENDPOINTS } from '@/lib/endpoints';

export type Role = 'owner' | 'admin' | 'member' | 'viewer';

export type AuthFlowKind = 'redirect' | 'phone_otp' | (string & {});

export interface AuthMethodDescriptor {
  id: string;
  kind: AuthFlowKind;
  login_url?: string;
  send_code_url?: string;
  verify_code_url?: string;
}

export interface SendPhoneOTPResponse {
  expires_in: number;
  cooldown_seconds: number;
}

export interface PhoneOTPLoginResponse {
  user: UserSummary;
  expires_at: string;
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

export function useCapabilities(enabled = true) {
  return useQuery({
    queryKey: authKeys.capabilities,
    queryFn: ({ signal }) => fetchCapabilities(signal),
    staleTime: 5 * 60_000,
    enabled,
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

// 报告后端是否启用了点击统计（FEATURE_ANALYTICS）；加载中或能力接口异常时保守返回 false，避免统计关闭时首屏闪烁。
export function useAnalyticsEnabled(): boolean {
  const { data } = useCapabilities();
  return Boolean(data?.features.analytics);
}

// 报告后端是否启用了异步邀请邮件（SMTP_HOST）；加载中或能力接口异常时保守返回 false。
export function useEmailEnabled(enabled = true): boolean {
  const { data } = useCapabilities(enabled);
  return Boolean(data?.features.email);
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

export function useSendPhoneOTP() {
  return useMutation({
    mutationFn: ({ url, phone }: { url: string; phone: string }) =>
      apiRequest<SendPhoneOTPResponse>(url, {
        method: 'POST',
        body: { phone },
        skipAuthRedirect: true,
      }),
  });
}

export function useVerifyPhoneOTP() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ url, phone, code }: { url: string; phone: string; code: string }) =>
      apiRequest<PhoneOTPLoginResponse>(url, {
        method: 'POST',
        body: { phone, code },
        skipAuthRedirect: true,
      }),
    onSuccess: () => {
      // 清掉登录页缓存的 401 状态，让目标页守卫用新 sl_session 拉取最新会话，避免双重跳转竞态。
      queryClient.removeQueries({ queryKey: authKeys.me });
    },
  });
}
