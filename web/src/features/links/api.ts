import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { authKeys } from '@/features/auth/api';
import { apiRequest } from '@/lib/api-client';
import { DEFAULT_LINKS_PAGE_SIZE } from '@/lib/constants';
import { ENDPOINTS } from '@/lib/endpoints';

export type LinkStatus = 'active' | 'disabled' | 'expired' | 'banned';
export type LinkStatusFilter = '' | LinkStatus;
export type LinkSort = 'id' | 'clicks' | 'clicks_today';

export interface AdminLink {
  code: string;
  url: string;
  status: LinkStatus;
  expires_at?: string;
  disabled_at?: string;
  banned_at?: string;
  ban_reason?: string;
  created_at: string;
  clicks: number;
  clicks_today: number;
}

export interface ListLinksResponse {
  items: AdminLink[];
  next_cursor?: string;
}

export interface LinkStats {
  code: string;
  total_clicks: number;
  range_clicks: number;
  from: string;
  to: string;
  clicks: Array<{ day: string; clicks: number }>;
  referrers: Array<{ referrer: string; clicks: number }>;
  devices: Array<{ device: string; clicks: number }>;
  clients: Array<{ client: string; clicks: number }>;
  daily_visitors: Array<{ day: string; visitors: number }>;
  unique_visitors: number;
}

export const linkKeys = {
  all: (orgId: number) => ['orgs', orgId, 'links'] as const,
  list: (orgId: number, sort: LinkSort, status: LinkStatusFilter, q: string) =>
    ['orgs', orgId, 'links', 'list', sort, status, q] as const,
  detail: (orgId: number, code: string) => ['orgs', orgId, 'links', 'detail', code] as const,
  stats: (orgId: number, code: string, days: number) =>
    ['orgs', orgId, 'links', 'stats', code, days] as const,
};

export function fetchLinks(
  orgId: number,
  params: {
    limit?: number;
    sort?: LinkSort;
    status?: LinkStatusFilter;
    q?: string;
    cursor?: string;
  },
  signal?: AbortSignal,
): Promise<ListLinksResponse> {
  const q = new URLSearchParams();
  if (params.limit) q.set('limit', String(params.limit));
  if (params.sort) q.set('sort', params.sort);
  if (params.status) q.set('status', params.status);
  if (params.q) q.set('q', params.q);
  if (params.cursor) q.set('cursor', params.cursor);
  const suffix = q.toString() ? `?${q.toString()}` : '';
  return apiRequest<ListLinksResponse>(`${ENDPOINTS.links.listCreate}${suffix}`, { orgId, signal });
}

export function fetchLinkDetail(
  orgId: number,
  code: string,
  signal?: AbortSignal,
): Promise<AdminLink> {
  return apiRequest<AdminLink>(ENDPOINTS.links.detail(code), { orgId, signal });
}

export function fetchLinkStats(
  orgId: number,
  code: string,
  days: number,
  signal?: AbortSignal,
): Promise<LinkStats> {
  const to = new Date();
  const from = new Date(to.getTime() - Math.max(days - 1, 0) * 24 * 60 * 60 * 1000);
  const q = new URLSearchParams({
    from: from.toISOString(),
    to: to.toISOString(),
  });
  return apiRequest<LinkStats>(`${ENDPOINTS.links.stats(code)}?${q.toString()}`, {
    orgId,
    signal,
  });
}

/*
  useInfiniteQuery 是 TanStack Query 对游标分页（cursor pagination）的原生抽象：
  每一页返回 next_cursor 作为下一页的 pageParam，切换 orgId、sort、status 或 q 时自动重置分页并取消旧请求。
*/
export function useInfiniteLinks(
  orgId: number,
  sort: LinkSort,
  status: LinkStatusFilter,
  q: string,
) {
  return useInfiniteQuery({
    queryKey: linkKeys.list(orgId, sort, status, q),
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam, signal }) =>
      fetchLinks(
        orgId,
        {
          limit: DEFAULT_LINKS_PAGE_SIZE,
          sort,
          status: status || undefined,
          q: q || undefined,
          cursor: pageParam,
        },
        signal,
      ),
    getNextPageParam: (lastPage) => lastPage.next_cursor,
  });
}

export function useLinkDetail(orgId: number, code: string) {
  return useQuery({
    queryKey: linkKeys.detail(orgId, code),
    queryFn: ({ signal }) => fetchLinkDetail(orgId, code, signal),
    enabled: Boolean(code),
  });
}

export function useLinkStats(orgId: number, code: string, days: 7 | 30) {
  return useQuery({
    queryKey: linkKeys.stats(orgId, code, days),
    queryFn: ({ signal }) => fetchLinkStats(orgId, code, days, signal),
    enabled: Boolean(code),
  });
}

export function useCreateLink(orgId: number) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (payload: { url: string; expires_at?: string | null }) =>
      apiRequest<AdminLink>(ENDPOINTS.links.listCreate, {
        method: 'POST',
        orgId,
        body: payload,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: linkKeys.all(orgId) });
      // 同步刷新 /me 让侧边栏与概览区的本月配额用量实时更新。
      void queryClient.invalidateQueries({ queryKey: authKeys.me });
    },
  });
}

export function useUpdateLink(orgId: number) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({
      code,
      payload,
    }: {
      code: string;
      payload: { url?: string; expires_at?: string | null; disabled?: boolean };
    }) =>
      apiRequest<AdminLink | undefined>(ENDPOINTS.links.detail(code), {
        method: 'PATCH',
        orgId,
        body: payload,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: linkKeys.all(orgId) });
    },
  });
}

export function useDeleteLink(orgId: number) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (code: string) =>
      apiRequest<void>(ENDPOINTS.links.detail(code), {
        method: 'DELETE',
        orgId,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: linkKeys.all(orgId) });
    },
  });
}
