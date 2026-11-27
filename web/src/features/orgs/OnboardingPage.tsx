import { type FormEvent, useState } from 'react';
import { ArrowRight, LogOut } from 'lucide-react';
import { Navigate, useNavigate } from 'react-router';
import { toast } from 'sonner';
import { routes } from '@/app/routes';
import { BrandLogo } from '@/components/BrandLogo';
import { CourierCat } from '@/components/CourierCat';
import { ErrorAlert } from '@/components/ErrorAlert';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input, Label } from '@/components/ui/input';
import { Skeleton } from '@/components/ui/skeleton';
import { useLogout, useMe } from '@/features/auth/api';
import { getErrorMessage } from '@/lib/api-client';
import { MAX_ORG_NAME_BYTES } from '@/lib/constants';
import { useDocumentTitle } from '@/lib/use-document-title';
import { useCreateOrg } from './api';

export function OnboardingPage() {
  useDocumentTitle('初始化组织');
  const navigate = useNavigate();
  const meQuery = useMe();
  const createOrg = useCreateOrg();
  const logout = useLogout();
  const [name, setName] = useState('');
  const [formError, setFormError] = useState<string | null>(null);

  if (meQuery.isPending) {
    return (
      <div className="flex min-h-screen items-center justify-center bg-paper-dots p-6">
        <Skeleton className="h-72 w-full max-w-md rounded-2xl" />
      </div>
    );
  }

  if (meQuery.isError || !meQuery.data) {
    return <Navigate to={routes.login()} replace />;
  }

  const firstOrg = meQuery.data.organizations[0];
  if (firstOrg) {
    return <Navigate to={routes.orgLinks(firstOrg.id)} replace />;
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const trimmed = name.trim();
    if (!trimmed) {
      setFormError('请输入组织名称');
      return;
    }
    setFormError(null);
    try {
      const created = await createOrg.mutateAsync(trimmed);
      toast.success(`欢迎来到 ${created.name}`);
      navigate(routes.orgLinks(created.id), { replace: true });
    } catch (err) {
      setFormError(getErrorMessage(err));
    }
  }

  async function handleLogout() {
    try {
      await logout.mutateAsync();
      navigate(routes.login(), { replace: true });
    } catch (err) {
      toast.error(getErrorMessage(err));
    }
  }

  return (
    <div className="flex min-h-screen flex-col items-center justify-center bg-paper-dots px-4 py-12">
      <div className="w-full max-w-md space-y-5">
        <div className="flex items-center justify-between">
          <BrandLogo />
          <Button
            variant="ghost"
            size="sm"
            disabled={logout.isPending}
            onClick={() => void handleLogout()}
          >
            <LogOut className="size-3.5" />
            {logout.isPending ? '退出中…' : '退出'}
          </Button>
        </div>

        <Card className="border-[1.5px] border-ink ink-shadow">
          <CardHeader className="space-y-2 pb-3">
            <div className="flex items-center justify-between gap-2">
              <div className="space-y-1.5">
                <span className="hl-swipe-yellow inline-block px-2 py-0.5 text-xs font-semibold text-ink">
                  初始化工作台
                </span>
                <CardTitle className="text-xl">创建你的第一个组织</CardTitle>
              </div>
              <CourierCat pose="success" className="size-16 shrink-0" />
            </div>
            <CardDescription>
              每个组织拥有完全隔离的短链、统计、成员与 API{'\u00A0'}
              Key 空间。创建后你将自动成为所有者。
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <p className="text-xs text-muted-foreground">
              当前账号：
              <strong className="font-semibold text-ink">
                {meQuery.data.user.name ||
                  meQuery.data.user.email ||
                  `用户 #${meQuery.data.user.id}`}
              </strong>
            </p>
            <form className="space-y-4" onSubmit={handleSubmit}>
              <div className="space-y-2">
                <Label htmlFor="first-org-name">组织名称</Label>
                <Input
                  id="first-org-name"
                  placeholder="例如：Acme Studio"
                  maxLength={MAX_ORG_NAME_BYTES}
                  autoFocus
                  required
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                />
              </div>
              {formError && <ErrorAlert compact message={formError} />}
              <Button
                type="submit"
                variant="highlight"
                className="w-full"
                disabled={createOrg.isPending}
              >
                {createOrg.isPending ? '创建中…' : '创建组织并进入工作台'}
                <ArrowRight className="size-4" />
              </Button>
            </form>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
