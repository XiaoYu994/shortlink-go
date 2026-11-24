import { useMutation, useQueryClient } from '@tanstack/react-query';
import { authKeys, type MeResponse, type Role } from '@/features/auth/api';
import { apiRequest } from '@/lib/api-client';
import { DEFAULT_ORG_LINK_QUOTA } from '@/lib/constants';
import { ENDPOINTS } from '@/lib/endpoints';

export interface CreatedOrg {
  id: number;
  name: string;
  role: Role;
  created_at: string;
}

const LAST_ORG_STORAGE_KEY = 'shortlink_last_org_id';

export function getLastOrgId(): number | null {
  try {
    const raw = window.localStorage.getItem(LAST_ORG_STORAGE_KEY);
    if (!raw) return null;
    const id = Number(raw);
    return Number.isInteger(id) && id > 0 ? id : null;
  } catch {
    return null;
  }
}

export function setLastOrgId(id: number | null): void {
  try {
    if (id === null) {
      window.localStorage.removeItem(LAST_ORG_STORAGE_KEY);
    } else {
      window.localStorage.setItem(LAST_ORG_STORAGE_KEY, String(id));
    }
  } catch {
    // 隐私模式禁用 localStorage 时静默忽略，路由参数本身仍可正常工作。
  }
}

export function postCreateOrg(name: string): Promise<CreatedOrg> {
  return apiRequest<CreatedOrg>(ENDPOINTS.orgs.listCreate, {
    method: 'POST',
    body: { name },
  });
}

export function useCreateOrg() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => postCreateOrg(name),
    onSuccess: (created) => {
      /*
        先同步把新组织写入 authKeys.me 缓存，再触发后台 invalidate 校准。
        这样随后立刻跳转到新组织页面时，路由守卫能瞬间认出新组织，不会闪回旧组织。
      */
      queryClient.setQueryData<MeResponse>(authKeys.me, (prev) => {
        if (!prev) return prev;
        if (prev.organizations.some((o) => o.id === created.id)) return prev;
        return {
          ...prev,
          organizations: [
            ...prev.organizations,
            {
              id: created.id,
              name: created.name,
              role: created.role,
              link_usage: 0,
              link_quota: DEFAULT_ORG_LINK_QUOTA,
            },
          ],
        };
      });
      setLastOrgId(created.id);
      void queryClient.invalidateQueries({ queryKey: authKeys.me });
    },
  });
}
