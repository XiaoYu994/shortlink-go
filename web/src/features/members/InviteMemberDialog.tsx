import { type FormEvent, useState } from 'react';
import { toast } from 'sonner';
import { routes } from '@/app/routes';
import { CopyButton } from '@/components/CopyButton';
import { CourierCat } from '@/components/CourierCat';
import { ErrorAlert } from '@/components/ErrorAlert';
import {
  createInitialExpiration,
  type ExpirationSelection,
  resolveExpiration,
} from '@/components/expiration-helpers';
import { ExpirationPicker } from '@/components/ExpirationPicker';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import { Input, Label } from '@/components/ui/input';
import { type Role, useEmailEnabled } from '@/features/auth/api';
import { assignableRolesFor, ROLE_LABELS } from '@/features/auth/permissions';
import { getErrorMessage } from '@/lib/api-client';
import { cn } from '@/lib/cn';
import { type Invitation, useCreateInvitation } from './api';

const ROLE_DESCRIPTIONS: Record<Role, string> = {
  owner: '完全控制组织、成员、API\u00A0Key、Webhook 与审计日志',
  admin: '管理成员（除所有者）、短链、API\u00A0Key、Webhook 与审计日志',
  member: '创建、编辑、停用与删除短链，查看点击统计',
  viewer: '仅可浏览短链列表、访问统计与成员名单',
};

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
  const [expiration, setExpiration] = useState<ExpirationSelection>(() =>
    createInitialExpiration('create'),
  );
  const [formError, setFormError] = useState<string | null>(null);
  const [createdInv, setCreatedInv] = useState<Invitation | null>(null);

  const emailEnabled = useEmailEnabled(open);
  const createInvitation = useCreateInvitation(orgId);
  const roles = assignableRolesFor(actorRole);

  function handleOpenChange(nextOpen: boolean) {
    if (!nextOpen) {
      setEmail('');
      setRole('member');
      setExpiration(createInitialExpiration('create'));
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
    const exp = resolveExpiration(expiration);
    if (exp.action === 'error') {
      setFormError(exp.message);
      return;
    }
    setFormError(null);
    try {
      const created = await createInvitation.mutateAsync({
        email: trimmed,
        role,
        ...(exp.action === 'set' ? { expires_at: exp.iso } : {}),
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
              <div className="flex items-center gap-3">
                <CourierCat pose="success" className="size-14 shrink-0" />
                <div className="space-y-1">
                  <DialogTitle>邀请已登记</DialogTitle>
                  <DialogDescription>
                    {emailEnabled ? (
                      <>
                        系统将自动向{' '}
                        <strong className="font-mono text-ink">{createdInv.email}</strong>{' '}
                        发送邀请邮件；你也可以复制下方工作台登录入口发给对方，使用该邮箱登录后将自动以{' '}
                        {ROLE_LABELS[createdInv.role]} 身份加入组织。
                      </>
                    ) : (
                      <>
                        当前未开启自动邀请邮件，请复制下方登录入口发送给{' '}
                        <strong className="font-mono text-ink">{createdInv.email}</strong>
                        ，对方使用该邮箱登录后将自动以 {ROLE_LABELS[createdInv.role]} 身份加入组织。
                      </>
                    )}
                  </DialogDescription>
                </div>
              </div>
            </DialogHeader>

            <div className="flex items-center gap-2 rounded-xl border-[1.25px] border-ink bg-paper-warm/60 px-3 py-2.5">
              <code className="min-w-0 flex-1 truncate font-mono text-xs font-medium text-ink">
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
                  setExpiration(createInitialExpiration('create'));
                }}
              >
                继续邀请其他人
              </Button>
              <Button variant="highlight" onClick={() => handleOpenChange(false)}>
                完成
              </Button>
            </DialogFooter>
          </>
        ) : (
          <>
            <DialogHeader>
              <DialogTitle>邀请新成员</DialogTitle>
              <DialogDescription>对方使用受邀邮箱登录后将自动加入组织。</DialogDescription>
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
                <span className="block text-sm font-medium leading-none text-ink">组织角色</span>
                <div role="radiogroup" aria-label="组织角色" className="grid gap-2">
                  {roles.map((r) => {
                    const selected = role === r;
                    return (
                      <label
                        key={r}
                        className={cn(
                          'flex items-start gap-2.5 rounded-xl border-[1.25px] px-3 py-2 text-sm transition-colors cursor-pointer',
                          selected
                            ? 'border-ink bg-hl-yellow/40'
                            : 'border-border-soft bg-paper-warm/40 hover:border-ink/60',
                        )}
                      >
                        <input
                          type="radio"
                          name="invite-role"
                          value={r}
                          checked={selected}
                          onChange={() => setRole(r)}
                          className="mt-1 size-4 accent-ink"
                        />
                        <span className="min-w-0">
                          <span className="font-semibold text-ink">{ROLE_LABELS[r]}</span>
                          <span className="block text-xs text-ink-secondary">
                            {ROLE_DESCRIPTIONS[r]}
                          </span>
                        </span>
                      </label>
                    );
                  })}
                </div>
              </div>

              <ExpirationPicker
                id="invite-expires"
                label="邀请有效期（可选）"
                mode="create"
                value={expiration}
                onChange={setExpiration}
              />

              {formError && <ErrorAlert compact message={formError} />}

              <DialogFooter>
                <Button
                  variant="outline"
                  disabled={createInvitation.isPending}
                  onClick={() => handleOpenChange(false)}
                >
                  取消
                </Button>
                <Button type="submit" variant="highlight" disabled={createInvitation.isPending}>
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
