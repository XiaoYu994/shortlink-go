import {
  Activity,
  CornerDownRight,
  Github,
  KeyRound,
  Link2,
  MousePointerClick,
  ShieldCheck,
  Zap,
} from 'lucide-react';
import { Navigate, useSearchParams } from 'react-router';
import { ErrorAlert } from '@/components/ErrorAlert';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { getErrorMessage } from '@/lib/api-client';
import { ENDPOINTS } from '@/lib/endpoints';
import { useDocumentTitle } from '@/lib/use-document-title';
import { useAuthProviders, useMe } from './api';
import { sanitizeReturnTo } from './return-to';

export function LoginPage() {
  useDocumentTitle('登录');
  const [searchParams] = useSearchParams();
  const returnTo = sanitizeReturnTo(searchParams.get('return_to'));

  const meQuery = useMe();
  const providersQuery = useAuthProviders();

  if (meQuery.data) {
    return <Navigate to={returnTo} replace />;
  }

  const providers = providersQuery.data;
  const hasAnyProvider = Boolean(providers && (providers.oidc || providers.github));

  return (
    <div className="grid min-h-screen bg-white lg:grid-cols-2">
      {/* 左侧品牌与产品预览区（宽屏展示） */}
      <div className="relative hidden flex-col justify-between overflow-hidden border-r border-zinc-200/80 bg-zinc-50 bg-dot-grid p-10 lg:flex">
        <div
          aria-hidden="true"
          className="pointer-events-none absolute -top-32 -left-24 size-96 rounded-full bg-indigo-500/10 blur-3xl"
        />

        <div className="relative flex items-center gap-2.5">
          <div className="flex size-8 items-center justify-center rounded-lg bg-zinc-900 text-white shadow-xs">
            <Link2 className="size-4" />
          </div>
          <span className="text-base font-semibold tracking-tight text-zinc-950">Shortlink</span>
          <span className="rounded-md border border-zinc-200 bg-white px-1.5 py-0.5 font-mono text-[10px] text-zinc-500">
            Console
          </span>
        </div>

        <div className="relative max-w-md space-y-6">
          <div className="space-y-3">
            <h1 className="text-3xl font-semibold tracking-tight text-zinc-950 leading-tight">
              干净、可观测的多租户
              <br />
              短链管理控制台
            </h1>
            <p className="text-sm leading-relaxed text-zinc-500">
              从组织隔离、按角色细粒度授权，到 Redis 写穿缓存与异步 URL
              风控，一条短链背后的完整工程闭环。
            </p>
          </div>

          {/* 静态产品卡片预览：第一眼建立清爽的 Dub.co / Vercel 视觉预期 */}
          <div className="rounded-2xl border border-zinc-200/90 bg-white p-4 shadow-[0_8px_30px_rgba(0,0,0,0.04)]">
            <div className="flex items-center justify-between gap-3">
              <div className="flex min-w-0 items-center gap-3">
                <span className="flex size-9 shrink-0 items-center justify-center rounded-full bg-indigo-50 text-sm font-semibold text-indigo-700 ring-1 ring-inset ring-indigo-200/80">
                  G
                </span>
                <div className="min-w-0">
                  <div className="flex items-center gap-1.5 font-mono text-sm font-semibold text-zinc-950">
                    /r/Launch26
                    <span className="size-2 rounded-full bg-emerald-500 ring-2 ring-emerald-500/20" />
                  </div>
                  <div className="flex items-center gap-1 text-xs text-zinc-500">
                    <CornerDownRight className="size-3 shrink-0 text-zinc-400" />
                    <span className="truncate">https://github.com/shortlink/releases</span>
                  </div>
                </div>
              </div>
              <span className="inline-flex shrink-0 items-center gap-1.5 rounded-lg border border-zinc-200 bg-zinc-50 px-2.5 py-1 font-mono text-xs font-medium text-zinc-800">
                <MousePointerClick className="size-3.5 text-brand" />
                12.4k
                <span className="rounded bg-emerald-100 px-1 text-[10px] font-semibold text-emerald-700">
                  +342
                </span>
              </span>
            </div>

            {/* 迷你趋势预览线 */}
            <svg viewBox="0 0 320 44" className="mt-3.5 h-10 w-full" aria-hidden="true">
              <defs>
                <linearGradient id="login-spark" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="0%" stopColor="#4f46e5" stopOpacity="0.18" />
                  <stop offset="100%" stopColor="#4f46e5" stopOpacity="0" />
                </linearGradient>
              </defs>
              <path
                d="M 0 36 C 35 32, 55 24, 90 26 C 125 28, 150 12, 190 16 C 230 20, 260 6, 320 8 L 320 44 L 0 44 Z"
                fill="url(#login-spark)"
              />
              <path
                d="M 0 36 C 35 32, 55 24, 90 26 C 125 28, 150 12, 190 16 C 230 20, 260 6, 320 8"
                fill="none"
                stroke="#4f46e5"
                strokeWidth="2"
                strokeLinecap="round"
              />
            </svg>
          </div>

          <div className="grid grid-cols-3 gap-3 text-xs text-zinc-600">
            <div className="flex items-center gap-1.5">
              <Zap className="size-3.5 text-brand" />
              Redis 写穿缓存
            </div>
            <div className="flex items-center gap-1.5">
              <Activity className="size-3.5 text-brand" />
              异步 URL 风控
            </div>
            <div className="flex items-center gap-1.5">
              <ShieldCheck className="size-3.5 text-brand" />
              多租户 RBAC
            </div>
          </div>
        </div>

        <div className="relative text-xs text-zinc-400">
          Go + PostgreSQL + Redis · React 19 + TanStack Query
        </div>
      </div>

      {/* 右侧极简登录区 */}
      <div className="flex flex-col items-center justify-center px-6 py-12">
        <div className="w-full max-w-sm space-y-6">
          <div className="flex flex-col items-center text-center lg:items-start lg:text-left">
            <div className="mb-3 flex size-10 items-center justify-center rounded-xl bg-zinc-900 text-white shadow-xs lg:hidden">
              <Link2 className="size-5" />
            </div>
            <h2 className="text-2xl font-semibold tracking-tight text-zinc-950">欢迎回来</h2>
            <p className="mt-1 text-sm text-zinc-500">选择已配置的身份提供方登录你的组织工作区</p>
          </div>

          <Card className="border-zinc-200/90 shadow-none">
            <CardHeader className="pb-3">
              <CardTitle className="text-sm font-medium text-zinc-700">身份验证方式</CardTitle>
              <CardDescription className="text-xs">
                登录后可自助创建组织、管理短链、邀请成员与签发 API Key。
              </CardDescription>
            </CardHeader>
            <CardContent className="space-y-2.5">
              {providersQuery.isPending && (
                <div className="space-y-2.5">
                  <Skeleton className="h-10 w-full" />
                  <Skeleton className="h-10 w-full" />
                </div>
              )}

              {providersQuery.isError && (
                <ErrorAlert
                  compact
                  message={`无法获取登录方式：${getErrorMessage(providersQuery.error)}`}
                />
              )}

              {providers && !hasAnyProvider && (
                <div className="rounded-lg border border-amber-200 bg-amber-50 px-3.5 py-3 text-sm text-amber-900">
                  当前 <code className="font-mono text-xs">console</code> 未启用 OIDC 或 GitHub
                  登录。请在服务端配置 <code className="font-mono text-xs">OIDC_*</code> 或{' '}
                  <code className="font-mono text-xs">GITHUB_*</code> 后重启。
                </div>
              )}

              {providers?.oidc && (
                <a
                  href={ENDPOINTS.auth.oidcLogin(returnTo)}
                  className="inline-flex h-10 w-full items-center justify-center gap-2 rounded-lg bg-zinc-900 px-4 text-sm font-medium text-white shadow-xs transition-colors hover:bg-zinc-800"
                >
                  <ShieldCheck className="size-4" />
                  使用 OIDC 单点登录（Dex / SSO）
                </a>
              )}

              {providers?.github && (
                <a
                  href={ENDPOINTS.auth.githubLogin(returnTo)}
                  className="inline-flex h-10 w-full items-center justify-center gap-2 rounded-lg border border-zinc-200 bg-white px-4 text-sm font-medium text-zinc-900 shadow-2xs transition-colors hover:bg-zinc-50"
                >
                  <Github className="size-4" />
                  使用 GitHub 账号登录
                </a>
              )}
            </CardContent>
          </Card>

          <p className="flex items-center justify-center gap-1.5 text-xs text-zinc-400 lg:justify-start">
            <KeyRound className="size-3.5" />受 HttpOnly 会话与同源安全策略保护
          </p>
        </div>
      </div>
    </div>
  );
}
