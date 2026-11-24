import { type FormEvent, useState } from 'react';
import { CheckCircle2 } from 'lucide-react';
import { toast } from 'sonner';
import { routes } from '@/app/routes';
import { CopyButton } from '@/components/CopyButton';
import { ErrorAlert } from '@/components/ErrorAlert';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import { Input, Label, Select } from '@/components/ui/input';
import type { Role } from '@/features/auth/api';
import { assignableRolesFor, ROLE_LABELS } from '@/features/auth/permissions';
import { getErrorMessage } from '@/lib/api-client';
import { localDatetimeToISO } from '@/lib/format';
import { type Invitation, useCreateInvitation } from './api';

interface InviteMemberDialogProps {
  orgId: number;
  actorRole: Role;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

export function InviteMemberDialog({
  orgId,
  actorRole,
  open,
  onOpenChange,
}: InviteMemberDialogProps) {
  const [email, setEmail] = useState('');
  const [role, setRole] = useState<Role>('member');
  const [expiresAt, setExpiresAt] = useState('');
  const [formError, setFormError] = useState<string | null>(null);
  const [createdInv, setCreatedInv] = useState<Invitation | null>(null);

  const createInvitation = useCreateInvitation(orgId);
  const roles = assignableRolesFor(actorRole);

  function handleOpenChange(nextOpen: boolean) {
    if (!nextOpen) {
      setEmail('');
      setRole('member');
      setExpiresAt('');
      setFormError(null);
      setCreatedInv(null);
    }
    onOpenChange(nextOpen);
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const trimmed = email.trim();
    if (!trimmed) {
      setFormError('请输入受邀邮箱');
      return;
    }
    setFormError(null);
    try {
      const iso = localDatetimeToISO(expiresAt);
      const created = await createInvitation.mutateAsync({
        email: trimmed,
        role,
        ...(iso ? { expires_at: iso } : {}),
      });
      toast.success(`已登记对 ${created.email} 的邀请`);
      setCreatedInv(created);
    } catch (err) {
      setFormError(getErrorMessage(err));
    }
  }

  const loginUrl = `${window.location.origin}${routes.login()}`;

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent>
        {createdInv ? (
          <>
            <DialogHeader>
              <div className="flex items-center gap-2 text-emerald-700">
                <CheckCircle2 className="size-5" />
                <DialogTitle>邀请已登记</DialogTitle>
              </div>
              <DialogDescription>
                当前暂未开启自动邮件通知。请将下方控制台登录入口发给{' '}
                <strong className="font-mono text-zinc-900">{createdInv.email}</strong>
                ，对方使用该已验证邮箱登录后将自动以 {ROLE_LABELS[createdInv.role]} 身份加入组织。
              </DialogDescription>
            </DialogHeader>

            <div className="flex items-center gap-2 rounded-lg border border-zinc-200 bg-zinc-50 px-3 py-2.5">
              <code className="min-w-0 flex-1 truncate font-mono text-xs text-zinc-800">
                {loginUrl}
              </code>
              <CopyButton
                value={loginUrl}
                variant="pill"
                size="sm"
                label="复制登录入口"
                toastMessage="已复制登录入口地址"
              />
            </div>

            <DialogFooter>
              <Button
                variant="outline"
                onClick={() => {
                  setCreatedInv(null);
                  setEmail('');
                  setExpiresAt('');
                }}
              >
                继续邀请其他人
              </Button>
              <Button onClick={() => handleOpenChange(false)}>完成</Button>
            </DialogFooter>
          </>
        ) : (
          <>
            <DialogHeader>
              <DialogTitle>邀请新成员</DialogTitle>
              <DialogDescription>
                登记受邀人邮箱与角色；对方下次使用该已验证邮箱登录控制台时将自动加入本组织。
              </DialogDescription>
            </DialogHeader>
            <form className="space-y-4" onSubmit={handleSubmit}>
              <div className="space-y-2">
                <Label htmlFor="invite-email">受邀人邮箱</Label>
                <Input
                  id="invite-email"
                  type="email"
                  required
                  autoFocus
                  placeholder="teammate@example.com"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                />
              </div>

              <div className="space-y-2">
                <Label htmlFor="invite-role">组织角色</Label>
                <Select
                  id="invite-role"
                  className="w-full"
                  value={role}
                  onChange={(e) => setRole(e.target.value as Role)}
                >
                  {roles.map((r) => (
                    <option key={r} value={r}>
                      {ROLE_LABELS[r]} ({r})
                    </option>
                  ))}
                </Select>
              </div>

              <div className="space-y-2">
                <Label htmlFor="invite-expires">过期时间（可选）</Label>
                <Input
                  id="invite-expires"
                  type="datetime-local"
                  value={expiresAt}
                  onChange={(e) => setExpiresAt(e.target.value)}
                />
              </div>

              {formError && <ErrorAlert compact message={formError} />}

              <DialogFooter>
                <Button
                  variant="outline"
                  disabled={createInvitation.isPending}
                  onClick={() => handleOpenChange(false)}
                >
                  取消
                </Button>
                <Button type="submit" disabled={createInvitation.isPending}>
                  {createInvitation.isPending ? '提交中…' : '发起邀请'}
                </Button>
              </DialogFooter>
            </form>
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}
