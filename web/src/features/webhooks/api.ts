import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiRequest } from '@/lib/api-client';
import { DEFAULT_WEBHOOK_DELIVERIES_PAGE_SIZE } from '@/lib/constants';
import { ENDPOINTS } from '@/lib/endpoints';

export type WebhookEvent =
  'link.created' | 'link.updated' | 'link.deleted' | 'link.banned' | 'link.unbanned';

export type WebhookDeliveryEvent = WebhookEvent | 'webhook.ping';
export type WebhookDeliveryStatus = 'success' | 'retry' | 'failed';
export type WebhookDisabledReason = '' | 'manual' | 'consecutive_failures';

export interface WebhookSubscription {
  id: number;
  url: string;
  description?: string;
  events: WebhookEvent[];
  secret_prefix: string;
  enabled: boolean;
  consecutive_failures: number;
  disabled_reason?: WebhookDisabledReason;
  disabled_at?: string;
  created_by?: number;
  created_at: string;
  updated_at: string;
}

export interface WebhookWithSecret extends WebhookSubscription {
  secret: string;
}

export interface WebhookDelivery {
  id: number;
  webhook_id: number;
  job_id: number;
  delivery_id: string;
  event: WebhookDeliveryEvent;
  attempt: number;
  status: WebhookDeliveryStatus;
  http_status?: number;
  duration_ms: number;
  error_message?: string;
  response_snippet?: string;
  created_at: string;
}

export interface ListWebhookDeliveriesResponse {
  items: WebhookDelivery[];
  next_cursor?: string;
}

export interface TestWebhookResponse {
  job_id: number;
  delivery_id: string;
}

export const ALL_WEBHOOK_EVENTS: ReadonlyArray<{
  value: WebhookEvent;
  label: string;
  description: string;
}> = [
  { value: 'link.created', label: 'link.created', description: '新建短链提交成功后触发' },
  { value: 'link.updated', label: 'link.updated', description: '修改短链地址、过期时间或启停状态' },
  { value: 'link.deleted', label: 'link.deleted', description: '删除非封禁短链时触发' },
  { value: 'link.banned', label: 'link.banned', description: '短链首次被封禁（风控或运维下架）' },
  { value: 'link.unbanned', label: 'link.unbanned', description: '已封禁短链被解封恢复' },
];

export const webhookKeys = {
  list: (orgId: number) => ['orgs', orgId, 'webhooks'] as const,
  deliveries: (orgId: number, webhookId: number) =>
    ['orgs', orgId, 'webhooks', webhookId, 'deliveries'] as const,
};

export function useWebhooks(orgId: number, enabled: boolean) {
  return useQuery({
    queryKey: webhookKeys.list(orgId),
    queryFn: async ({ signal }) => {
      const res = await apiRequest<{ items: WebhookSubscription[] }>(
        ENDPOINTS.webhooks.listCreate,
        {
          orgId,
          signal,
        },
      );
      return res.items;
    },
    enabled,
  });
}

export function useCreateWebhook(orgId: number) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (payload: { url: string; description?: string; events: WebhookEvent[] }) =>
      apiRequest<WebhookWithSecret>(ENDPOINTS.webhooks.listCreate, {
        method: 'POST',
        orgId,
        body: payload,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: webhookKeys.list(orgId) });
    },
  });
}

export function useUpdateWebhook(orgId: number) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (vars: {
      id: number;
      url?: string;
      description?: string;
      events?: WebhookEvent[];
      enabled?: boolean;
    }) => {
      const { id, ...body } = vars;
      return apiRequest<WebhookSubscription>(ENDPOINTS.webhooks.detail(id), {
        method: 'PATCH',
        orgId,
        body,
      });
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: webhookKeys.list(orgId) });
    },
  });
}

export function useDeleteWebhook(orgId: number) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) =>
      apiRequest<void>(ENDPOINTS.webhooks.detail(id), {
        method: 'DELETE',
        orgId,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: webhookKeys.list(orgId) });
    },
  });
}

export function useRotateWebhookSecret(orgId: number) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) =>
      apiRequest<WebhookWithSecret>(ENDPOINTS.webhooks.rotateSecret(id), {
        method: 'POST',
        orgId,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: webhookKeys.list(orgId) });
    },
  });
}

export function useTestWebhook(orgId: number) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) =>
      apiRequest<TestWebhookResponse>(ENDPOINTS.webhooks.test(id), {
        method: 'POST',
        orgId,
      }),
    onSuccess: (_res, id) => {
      void queryClient.invalidateQueries({ queryKey: webhookKeys.deliveries(orgId, id) });
    },
  });
}

export function useInfiniteWebhookDeliveries(
  orgId: number,
  webhookId: number | null,
  enabled: boolean,
) {
  return useInfiniteQuery({
    queryKey: webhookKeys.deliveries(orgId, webhookId ?? 0),
    initialPageParam: undefined as string | undefined,
    enabled: enabled && webhookId !== null && webhookId > 0,
    staleTime: 0,
    queryFn: ({ pageParam, signal }) => {
      const q = new URLSearchParams();
      q.set('limit', String(DEFAULT_WEBHOOK_DELIVERIES_PAGE_SIZE));
      if (pageParam) q.set('cursor', pageParam);
      return apiRequest<ListWebhookDeliveriesResponse>(
        `${ENDPOINTS.webhooks.deliveries(webhookId ?? 0)}?${q.toString()}`,
        { orgId, signal },
      );
    },
    getNextPageParam: (lastPage) => lastPage.next_cursor,
  });
}
