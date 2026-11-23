import { useEffect } from 'react';
import { Navigate, useLocation, useParams } from 'react-router';
import { Button } from '@/components/ui/button';
import { Skeleton } from '@/components/ui/skeleton';
import { useMe } from '@/features/auth/api';
import { getLastOrgId, setLastOrgId } from '@/features/orgs/api';
import { APIError, getErrorMessage } from '@/lib/api-client';
import { ConsoleLayout } from './ConsoleLayout';
import { OrgContext } from './org-context';

function FullPageLoading() {
  return (
    <div className="flex min-h-screen items-center justify-center bg-background p-6">
      <div className="w-full max-w-sm space-y-3">
        <Skeleton className="h-8 w-40" />
        <Skeleton className="h-32 w-full rounded-xl" />
      </div>
    </div>
  );
}

function buildLoginRedirect(pathname: string, search: string): string {
  const full = pathname + search;
  if (!full || full === '/' || full.startsWith('/login')) {
    return '/login';
  }
  return `/login?return_to=${encodeURIComponent(full)}`;
}

// 根路径 `/`：根据会话与组织归属自动跳转到登录、建组织引导或上次访问的组织。
export function RootRedirect() {
  const meQuery = useMe();

  if (meQuery.isPending) return <FullPageLoading />;

  if (meQuery.isError) {
    if (meQuery.error instanceof APIError && (meQuery.error.status === 401 || meQuery.error.status === 404)) {
      return <Navigate to="/login" replace />;
    }
    return (
      <div className="flex min-h-screen flex-col items-center justify-center gap-3 p-6 text-center">
        <p className="text-sm text-destructive">无法连接控制台：{getErrorMessage(meQuery.error)}</p>
        <Button variant="outline" onClick={() => void meQuery.refetch()}>
          重试
        </Button>
      </div>
    );
  }

  const orgs = meQuery.data.organizations;
  if (orgs.length === 0) {
    return <Navigate to="/onboarding" replace />;
  }

  const lastId = getLastOrgId();
  const target = orgs.find((o) => o.id === lastId) ?? orgs[0];
  return <Navigate to={`/orgs/${target.id}/links`} replace />;
}

/*
  OrgRouteGuard 保护所有 /orgs/:orgId/* 路由：
  1. 校验登录会话（未登录带 return_to 跳 /login，登录后由后端 302 精确送回原页面）；
  2. 校验 :orgId 属于当前用户成员列表（防止手敲或被移出组织后停留在失效路径）；
  3. 通过 OrgContext 向子页面提供当前组织与角色。
*/
export function OrgRouteGuard() {
  const { orgId: rawOrgId } = useParams();
  const location = useLocation();
  const meQuery = useMe();

  const parsedOrgId = Number(rawOrgId);
  const activeOrg =
    meQuery.data?.organizations.find((o) => o.id === parsedOrgId) ?? null;

  useEffect(() => {
    if (activeOrg) {
      setLastOrgId(activeOrg.id);
    }
  }, [activeOrg]);

  if (meQuery.isPending) return <FullPageLoading />;

  if (meQuery.isError) {
    if (meQuery.error instanceof APIError && (meQuery.error.status === 401 || meQuery.error.status === 404)) {
      return <Navigate to={buildLoginRedirect(location.pathname, location.search)} replace />;
    }
    return (
      <div className="flex min-h-screen flex-col items-center justify-center gap-3 p-6 text-center">
        <p className="text-sm text-destructive">加载会话失败：{getErrorMessage(meQuery.error)}</p>
        <Button variant="outline" onClick={() => void meQuery.refetch()}>
          重试
        </Button>
      </div>
    );
  }

  const { user, organizations } = meQuery.data;
  if (organizations.length === 0) {
    return <Navigate to="/onboarding" replace />;
  }

  if (!activeOrg) {
    return <Navigate to={`/orgs/${organizations[0].id}/links`} replace />;
  }

  return (
    <OrgContext.Provider value={{ user, org: activeOrg, organizations }}>
      <ConsoleLayout />
    </OrgContext.Provider>
  );
}
