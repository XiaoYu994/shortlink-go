import { type FormEvent, useState } from 'react';
import { ArrowRight, Building2, LogOut } from 'lucide-react';
import { Navigate, useNavigate } from 'react-router';
import { toast } from 'sonner';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input, Label } from '@/components/ui/input';
import { Skeleton } from '@/components/ui/skeleton';
import { useLogout, useMe } from '@/features/auth/api';
import { getErrorMessage } from '@/lib/api-client';
import { useCreateOrg } from './api';

export function OnboardingPage() {
  const navigate = useNavigate();
  const meQuery = useMe();
  const createOrg = useCreateOrg();
  const logout = useLogout();
  const [name, setName] = useState('');
  const [formError, setFormError] = useState<string | null>(null);

  if (meQuery.isPending) {
    return (
      <div className="flex min-h-screen items-center justify-center bg-zinc-50 p-6">
        <Skeleton className="h-64 w-full max-w-md rounded-2xl" />
      </div>
    );
  }

  if (meQuery.isError || !meQuery.data) {
    return <Navigate to="/login" replace />;
  }

  if (meQuery.data.organizations.length > 0) {
    return <Navigate to={`/orgs/${meQuery.data.organizations[0].id}/links`} replace />;
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
      navigate(`/orgs/${created.id}/links`, { replace: true });
    } catch (err) {
      setFormError(getErrorMessage(err));
    }
  }

  async function handleLogout() {
    await logout.mutateAsync();
    navigate('/login', { replace: true });
  }

  return (
    <div className="flex min-h-screen flex-col items-center justify-center bg-zinc-50 bg-dot-grid px-4 py-12">
      <div className="w-full max-w-md space-y-5">
        <div className="flex items-center justify-between text-xs text-zinc-500">
          <span>
            当前账号：
            <strong className="font-medium text-zinc-800">
              {meQuery.data.user.name || meQuery.data.user.email || `用户 #${meQuery.data.user.id}`}
            </strong>
          </span>
          <Button variant="ghost" size="sm" onClick={() => void handleLogout()}>
            <LogOut className="size-3.5" />
            退出
          </Button>
        </div>

        <Card className="rounded-2xl border-zinc-200/90 shadow-[0_8px_30px_rgba(0,0,0,0.04)]">
          <CardHeader className="space-y-2 pb-4">
            <div className="inline-flex w-fit items-center gap-1.5 rounded-full border border-indigo-200 bg-indigo-50 px-2.5 py-0.5 text-[11px] font-medium text-brand">
              <Building2 className="size-3" />
              步骤 1 / 1 · 初始化工作区
            </div>
            <CardTitle className="text-xl">创建你的第一个组织</CardTitle>
            <CardDescription>
              每个组织拥有完全隔离的短链、统计、成员与 API Key 空间。创建后你将自动成为 Owner。
            </CardDescription>
          </CardHeader>
          <CardContent>
            <form className="space-y-4" onSubmit={handleSubmit}>
              <div className="space-y-2">
                <Label htmlFor="first-org-name">组织名称</Label>
                <Input
                  id="first-org-name"
                  placeholder="例如：Acme Studio"
                  maxLength={100}
                  autoFocus
                  required
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                />
              </div>
              {formError && (
                <div className="rounded-md border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700">
                  {formError}
                </div>
              )}
              <Button type="submit" className="w-full" disabled={createOrg.isPending}>
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
