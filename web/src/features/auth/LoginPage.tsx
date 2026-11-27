import { Activity, Github, KeyRound, ShieldCheck, Users } from 'lucide-react';
import { Navigate, useSearchParams } from 'react-router';
import { BrandLogo } from '@/components/BrandLogo';
import { CourierCat } from '@/components/CourierCat';
import { ErrorAlert } from '@/components/ErrorAlert';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { getErrorMessage } from '@/lib/api-client';
import { ENDPOINTS } from '@/lib/endpoints';
import { useDocumentTitle } from '@/lib/use-document-title';
import { type AuthMethodDescriptor, useCapabilities, useMe } from './api';
import { PhoneOTPLoginForm } from './PhoneOTPLoginForm';
import { sanitizeReturnTo } from './return-to';

function isRedirectMethod(
  m: AuthMethodDescriptor,
): m is AuthMethodDescriptor & { login_url: string } {
  return m.kind === 'redirect' && Boolean(m.login_url);
}

function isPhoneOTPMethod(
  m: AuthMethodDescriptor,
): m is AuthMethodDescriptor & { send_code_url: string; verify_code_url: string } {
  return m.kind === 'phone_otp' && Boolean(m.send_code_url) && Boolean(m.verify_code_url);
}

const REDIRECT_METHOD_META: Record<
  string,
  { label: string; icon: typeof ShieldCheck; primary?: boolean }
> = {
  oidc: { label: '使用单点登录（SSO）', icon: ShieldCheck, primary: true },
  github: { label: '使用 GitHub 账号登录', icon: Github },
};

export function LoginPage() {
  useDocumentTitle('登录');
  const [searchParams] = useSearchParams();
  const returnTo = sanitizeReturnTo(searchParams.get('return_to'));

  const meQuery = useMe();
  const capabilitiesQuery = useCapabilities();

  if (meQuery.data) {
    return <Navigate to={returnTo} replace />;
  }

  const capabilities = capabilitiesQuery.data;
  const authMethods = capabilities?.auth_methods ?? [];
  const redirectMethods = authMethods.filter(isRedirectMethod);
  const phoneOTPMethods = authMethods.filter(isPhoneOTPMethod);
  const unsupportedMethods = authMethods.filter(
    (m) => !isRedirectMethod(m) && !isPhoneOTPMethod(m),
  );
  const hasAnyMethod = authMethods.length > 0;
  const hasInteractiveMethod = redirectMethods.length > 0 || phoneOTPMethods.length > 0;
  const checkingSession = meQuery.isPending;

  return (
    <div className="grid min-h-screen bg-paper lg:grid-cols-2">
      {/* 左侧品牌插画与产品叙事区（宽屏展示） */}
      <div className="relative hidden flex-col justify-between overflow-hidden border-r-[1.25px] border-ink/80 bg-paper-dots p-10 lg:flex">
        <BrandLogo size="md" />

        <div className="relative max-w-md space-y-6">
          <div className="space-y-3">
            <h1 className="text-3xl font-bold leading-tight tracking-tight text-ink">
              把长链接，
              <br />
              <span className="hl-swipe-yellow px-1.5">折叠成轻巧的纸飞机</span>
            </h1>
            <p className="text-sm leading-relaxed text-ink-secondary">
              多团队空间隔离、实时点击归因与自动安全风控，让每一条发出去的短链都清晰可控。
            </p>
          </div>

          <div className="rounded-2xl border-[1.25px] border-dashed border-ink/35 bg-card/70 px-4 py-3">
            <CourierCat pose="hero" />
          </div>

          <div className="grid grid-cols-3 gap-3 text-xs font-medium text-ink-secondary">
            <div className="flex items-center gap-1.5">
              <Users className="size-3.5 text-ink" />
              团队空间隔离
            </div>
            <div className="flex items-center gap-1.5">
              <Activity className="size-3.5 text-ink" />
              实时点击归因
            </div>
            <div className="flex items-center gap-1.5">
              <ShieldCheck className="size-3.5 text-ink" />
              自动安全风控
            </div>
          </div>
        </div>

        <div className="text-xs text-muted-foreground">Ori · 折羽短链工作台</div>
      </div>

      {/* 右侧手绘偏移登录区 */}
      <div className="flex flex-col items-center justify-center max-lg:bg-paper-dots px-6 py-12 lg:bg-paper">
        <div className="w-full max-w-sm space-y-6">
          <div className="flex flex-col items-center text-center lg:items-start lg:text-left">
            <div className="mb-4 lg:hidden">
              <BrandLogo size="md" />
            </div>
            <h2 className="text-2xl font-bold tracking-tight text-ink">欢迎回来</h2>
            <p className="mt-1 text-sm text-ink-secondary">选择一种方式登录</p>
          </div>

          <Card className="border-[1.5px] border-ink ink-shadow">
            <CardHeader className="pb-3">
              <CardTitle className="text-sm font-semibold text-ink">登录方式</CardTitle>
            </CardHeader>
            <CardContent className="space-y-3">
              {(checkingSession || capabilitiesQuery.isPending) && (
                <div className="space-y-2.5" aria-label="正在检查登录状态">
                  <Skeleton className="h-10 w-full" />
                  <Skeleton className="h-10 w-full" />
                </div>
              )}

              {!checkingSession && capabilitiesQuery.isError && (
                <ErrorAlert
                  compact
                  message={`无法获取登录方式：${getErrorMessage(capabilitiesQuery.error)}`}
                />
              )}

              {!checkingSession && capabilities && !hasAnyMethod && (
                <div className="rounded-xl border-[1.25px] border-ink/70 bg-hl-peach/35 px-3.5 py-3 text-sm text-ink">
                  暂未开放任何登录方式，请联系工作台管理员检查登录配置。
                </div>
              )}

              {!checkingSession &&
                redirectMethods.map((method, index) => {
                  const meta = REDIRECT_METHOD_META[method.id] ?? {
                    label: `使用 ${method.id.toUpperCase()} 登录`,
                    icon: KeyRound,
                  };
                  const Icon = meta.icon;
                  const isPrimary = meta.primary ?? index === 0;
                  return (
                    <a
                      key={method.id}
                      href={ENDPOINTS.auth.redirectLogin(method.login_url, returnTo)}
                      className={
                        isPrimary
                          ? 'inline-flex h-10 w-full items-center justify-center gap-2 rounded-xl border-[1.5px] border-ink bg-ink px-4 text-sm font-medium text-primary-foreground ink-shadow-sm ink-pressable hover:bg-ink/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink focus-visible:ring-offset-2'
                          : 'inline-flex h-10 w-full items-center justify-center gap-2 rounded-xl border-[1.25px] border-ink bg-card px-4 text-sm font-medium text-ink transition-colors hover:bg-paper-warm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink focus-visible:ring-offset-2'
                      }
                    >
                      <Icon className="size-4" />
                      {meta.label}
                    </a>
                  );
                })}

              {!checkingSession && redirectMethods.length > 0 && phoneOTPMethods.length > 0 && (
                <div className="relative py-1">
                  <div className="absolute inset-0 flex items-center" aria-hidden="true">
                    <span className="w-full border-t border-dashed border-ink/35" />
                  </div>
                  <div className="relative flex justify-center text-xs">
                    <span className="bg-card px-2 text-muted-foreground">或使用手机号验证码</span>
                  </div>
                </div>
              )}

              {!checkingSession &&
                phoneOTPMethods.map((method) => (
                  <PhoneOTPLoginForm
                    key={method.id}
                    id={method.id}
                    sendCodeUrl={method.send_code_url}
                    verifyCodeUrl={method.verify_code_url}
                    returnTo={returnTo}
                  />
                ))}

              {!checkingSession &&
                unsupportedMethods.length > 0 &&
                (!hasInteractiveMethod ? (
                  <div className="rounded-xl border-[1.25px] border-ink/70 bg-hl-peach/35 px-3.5 py-3 text-sm text-ink">
                    以下登录方式暂不支持在当前网页直接使用：
                    <code className="ml-1 font-mono text-xs">
                      {unsupportedMethods.map((m) => m.id).join('、')}
                    </code>
                  </div>
                ) : (
                  <p className="pt-1 text-xs text-muted-foreground">
                    另有暂不支持在当前网页直接使用的登录方式：
                    <code className="ml-1 font-mono text-[11px] text-ink-secondary">
                      {unsupportedMethods.map((m) => m.id).join('、')}
                    </code>
                  </p>
                ))}
            </CardContent>
          </Card>

          <p className="flex items-center justify-center gap-1.5 text-xs text-muted-foreground lg:justify-start">
            <KeyRound className="size-3.5" />
            全站加密传输与账号安全保护
          </p>
        </div>
      </div>
    </div>
  );
}
