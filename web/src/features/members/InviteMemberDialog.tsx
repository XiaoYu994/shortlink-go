import { type FormEvent, useState } from 'react';
import { toast } from 'sonner';
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
import { useCreateInvitation } from './api';

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

  const createInvitation = useCreateInvitation(orgId);
  const roles = assignableRolesFor(actorRole);

  function handleOpenChange(nextOpen: boolean) {
    if (!nextOpen) {
      setEmail('');
      setRole('member');
      setExpiresAt('');
      setFormError(null);
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
      toast.success(`已创建对 ${created.email} 的 ${ROLE_LABELS[created.role]} 邀请`);
      handleOpenChange(false);
    } catch (err) {
      setFormError(getErrorMessage(err));
    }
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>邀请新成员</DialogTitle>
          <DialogDescription>
            系统会在库内登记邀请记录；对方下次使用该已验证邮箱通过 OIDC 或 GitHub 登录时自动加入组织。
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

          {formError && (
            <div className="rounded-md border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700">
              {formError}
            </div>
          )}

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
      </DialogContent>
    </Dialog>
  );
}
