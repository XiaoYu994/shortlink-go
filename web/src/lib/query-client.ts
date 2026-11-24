import { MutationCache, QueryCache, QueryClient } from '@tanstack/react-query';
import { APIError } from './api-client';

export interface QueryClientOptions {
  // 当接口返回 403 或 organization not found 404 时回调，由装配层决定如何刷新会话权限视图。
  onOrgAccessDenied?: () => void;
}

function isOrgAccessDenied(error: unknown): boolean {
  if (!(error instanceof APIError)) return false;
  if (error.status === 403) return true;
  return error.status === 404 && error.rawMessage === 'organization not found';
}

/*
  TanStack Query 的全局实例工厂（类比后端里的缓存客户端 + 默认策略配置）。
  为什么在生产 React 中优先用 TanStack Query 而不是手写 useEffect + fetch：
  1. 自动按 queryKey 去重与缓存（切换 Tab 再切回来无需白屏重拉）；
  2. 自动透传 AbortSignal，解决快速切换组织/排序时的“旧请求晚返回覆盖新数据”竞态；
  3. 明确区分「服务端状态（来自 API）」与「客户端 UI 状态（弹窗开关、输入草稿）」。
*/
export function createQueryClient(opts: QueryClientOptions = {}): QueryClient {
  const handleError = (error: unknown) => {
    if (isOrgAccessDenied(error)) {
      opts.onOrgAccessDenied?.();
    }
  };

  return new QueryClient({
    queryCache: new QueryCache({ onError: handleError }),
    mutationCache: new MutationCache({ onError: handleError }),
    defaultOptions: {
      queries: {
        // 30 秒内视为新鲜，切回页面不会立刻闪烁重载；写操作后由 mutation 主动 invalidate。
        staleTime: 30_000,
        refetchOnWindowFocus: false,
        retry: (failureCount, error) => {
          // 4xx 属于业务或权限错误（如 401/403/404），重试毫无意义，立刻交给 UI 处理。
          if (error instanceof APIError && error.status >= 400 && error.status < 500) {
            return false;
          }
          return failureCount < 1;
        },
      },
      mutations: {
        retry: false,
      },
    },
  });
}
