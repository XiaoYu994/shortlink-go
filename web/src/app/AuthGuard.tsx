import { useEffect, useRef } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Navigate, useLocation, useNavigate, useParams } from 'react-router';
import { toast } from 'sonner';
import { ErrorAlert } from '@/components/ErrorAlert';
import { Skeleton } from '@/components/ui/skeleton';
import { useMe } from '@/features/auth/api';
import { getLastOrgId, setLastOrgId } from '@/features/orgs/api';
import { APIError, getErrorMessage, setUnauthorizedHandler } from '@/lib/api-client';
import { ConsoleLayout } from './ConsoleLayout';
import { OrgContext } from './org-context';
import { routes } from './routes';

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

// 根路径 `/`：根据会话与组织归属自动跳转到登录、建组织引导或上次访问的组织。
export function RootRedirect() {
  const meQuery = useMe();

  if (meQuery.isPending) return <FullPageLoading />;

  if (meQuery.isError) {
    if (
      meQuery.error instanceof APIError &&
      (meQuery.error.status === 401 || meQuery.error.status === 404)
    ) {
      return <Navigate to={routes.login()} replace />;
    }
    return (
      <div className="flex min-h-screen items-center justify-center p-6">
        <ErrorAlert
          className="w-full max-w-md"
          message={`无法连接控制台：${getErrorMessage(meQuery.error)}`}
          onRetry={() => void meQuery.refetch()}
        />
      </div>
    );
  }

  const firstOrg = meQuery.data.organizations[0];
  if (!firstOrg) {
    return <Navigate to={routes.onboarding()} replace />;
  }

  const lastId = getLastOrgId();
  const target = meQuery.data.organizations.find((o) => o.id === lastId) ?? firstOrg;
  return <Navigate to={routes.orgLinks(target.id)} replace />;
}

/*
  OrgRouteGuard 保护所有 /orgs/:orgId/* 路由：
  1. 校验登录会话（未登录带 return_to 跳登录）；
  2. 注册全局 401 监听：用户在工作台停留期间若会话过期或被其他标签页登出，自动清缓存并跳回登录页；
  3. 校验 :orgId 属于当前用户成员列表，并通过 OrgContext 向下传递组织与配额信息。
*/
export function OrgRouteGuard() {
  const { orgId: rawOrgId } = useParams();
  const location = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const meQuery = useMe();
  const handling401Ref = useRef(false);

  const parsedOrgId = Number(rawOrgId);
  const activeOrg = meQuery.data?.organizations.find((o) => o.id === parsedOrgId) ?? null;

  useEffect(() => {
    if (activeOrg) {
      setLastOrgId(activeOrg.id);
    }
  }, [activeOrg]);

  useEffect(() => {
    setUnauthorizedHandler(() => {
      if (handling401Ref.current) return;
      handling401Ref.current = true;
      queryClient.clear();
      toast.error('登录状态已过期，请重新登录');
      navigate(routes.login(location.pathname + location.search), { replace: true });
    });
    return () => {
      setUnauthorizedHandler(null);
    };
  }, [location.pathname, location.search, navigate, queryClient]);

  if (meQuery.isPending) return <FullPageLoading />;

  if (meQuery.isError) {
    if (
      meQuery.error instanceof APIError &&
      (meQuery.error.status === 401 || meQuery.error.status === 404)
    ) {
      return <Navigate to={routes.login(location.pathname + location.search)} replace />;
    }
    return (
      <div className="flex min-h-screen items-center justify-center p-6">
        <ErrorAlert
          className="w-full max-w-md"
          message={`加载会话失败：${getErrorMessage(meQuery.error)}`}
          onRetry={() => void meQuery.refetch()}
        />
      </div>
    );
  }

  const { user, organizations } = meQuery.data;
  const firstOrg = organizations[0];
  if (!firstOrg) {
    return <Navigate to={routes.onboarding()} replace />;
  }

  if (!activeOrg) {
    return <Navigate to={routes.orgLinks(firstOrg.id)} replace />;
  }

  return (
    <OrgContext.Provider value={{ user, org: activeOrg, organizations }}>
      <ConsoleLayout />
    </OrgContext.Provider>
  );
}
