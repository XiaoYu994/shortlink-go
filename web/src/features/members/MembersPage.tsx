import { useState } from 'react';
import { Mail, RefreshCw, Trash2, UserPlus, Users } from 'lucide-react';
import { toast } from 'sonner';
import { useCurrentOrg } from '@/app/org-context';
import { routes } from '@/app/routes';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { CopyButton } from '@/components/CopyButton';
import { EmptyState } from '@/components/EmptyState';
import { ErrorAlert } from '@/components/ErrorAlert';
import { MonogramAvatar } from '@/components/MonogramAvatar';
import { PageHeader } from '@/components/PageHeader';
import { Badge, type BadgeProps } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Select } from '@/components/ui/input';
import { Skeleton } from '@/components/ui/skeleton';
import type { Role } from '@/features/auth/api';
import {
  assignableRolesFor,
  canAssignRole,
  canManageMembers,
  ROLE_LABELS,
} from '@/features/auth/permissions';
import { getErrorMessage } from '@/lib/api-client';
import { cn } from '@/lib/cn';
import { formatDateTime, formatRelativeTime } from '@/lib/format';
import { useDocumentTitle } from '@/lib/use-document-title';
import {
  type Invitation,
  type InvitationStatus,
  type Member,
  useInvitations,
  useMembers,
  useRemoveMember,
  useRevokeInvitation,
  useUpdateMemberRole,
} from './api';
import { InviteMemberDialog } from './InviteMemberDialog';

const INVITATION_STATUS_META: Record<
  InvitationStatus,
  { label: string; variant: BadgeProps['variant'] }
> = {
  pending: { label: '待兑现', variant: 'warning' },
  accepted: { label: '已兑现', variant: 'success' },
  revoked: { label: '已撤销', variant: 'destructive' },
  expired: { label: '已过期', variant: 'secondary' },
};

export function MembersPage() {
  const { user, org } = useCurrentOrg();
  const canManage = canManageMembers(org.role);
  const assignableRoles = assignableRolesFor(org.role);

  useDocumentTitle(`成员与邀请 · ${org.name}`);

  const [section, setSection] = useState<'members' | 'invitations'>('members');
  const [inviteOpen, setInviteOpen] = useState(false);
  const [removingMember, setRemovingMember] = useState<Member | null>(null);
  const [revokingInv, setRevokingInv] = useState<Invitation | null>(null);

  const membersQuery = useMembers(org.id);
  const invitationsQuery = useInvitations(org.id);
  const updateRole = useUpdateMemberRole(org.id);
  const removeMember = useRemoveMember(org.id);
  const revokeInvitation = useRevokeInvitation(org.id);

  async function handleRoleChange(member: Member, nextRole: Role) {
    if (member.role === nextRole) return;
    try {
      await updateRole.mutateAsync({ userId: member.user_id, role: nextRole });
      toast.success(
        `已将 ${member.email || `用户 #${member.user_id}`} 角色改为 ${ROLE_LABELS[nextRole]}`,
      );
    } catch (err) {
      toast.error(getErrorMessage(err));
    }
  }

  async function handleConfirmRemove() {
    if (!removingMember) return;
    try {
      await removeMember.mutateAsync(removingMember.user_id);
      toast.success(`已移出成员 ${removingMember.email || `#${removingMember.user_id}`}`);
      setRemovingMember(null);
    } catch (err) {
      toast.error(getErrorMessage(err));
    }
  }

  async function handleConfirmRevoke() {
    if (!revokingInv) return;
    try {
      await revokeInvitation.mutateAsync(revokingInv.id);
      toast.success(`已撤销对 ${revokingInv.email} 的邀请`);
      setRevokingInv(null);
    } catch (err) {
      toast.error(getErrorMessage(err));
    }
  }

  const isRefreshing = membersQuery.isFetching || invitationsQuery.isFetching;
  const pendingInvCount =
    invitationsQuery.data?.filter((item) => item.status === 'pending').length ?? 0;

  return (
    <div className="space-y-6">
      <PageHeader
        title="成员与邀请"
        description="角色调整和移出成员会立即生效；为避免组织无人管理，系统至少会保留一位所有者。"
        actions={
          <>
            <Button
              variant="outline"
              size="sm"
              disabled={isRefreshing}
              onClick={() => {
                void membersQuery.refetch();
                void invitationsQuery.refetch();
              }}
            >
              <RefreshCw className={cn('size-3.5', isRefreshing && 'animate-spin')} />
              刷新
            </Button>
            {canManage && (
              <Button size="sm" onClick={() => setInviteOpen(true)}>
                <UserPlus className="size-4" />
                邀请成员
              </Button>
            )}
          </>
        }
      />

      {/* 分段切换：避免两张大表垂直堆叠显得臃肿 */}
      <div className="flex items-center justify-between border-b border-zinc-200/80">
        <div className="flex gap-1">
          <button
            type="button"
            onClick={() => setSection('members')}
            className={cn(
              'relative inline-flex items-center gap-2 px-3 pb-3 text-sm font-medium transition-colors cursor-pointer',
              section === 'members' ? 'text-zinc-950' : 'text-zinc-500 hover:text-zinc-900',
            )}
          >
            <Users className="size-4" />
            组织成员
            <span className="rounded-full bg-zinc-100 px-2 py-0.5 font-mono text-[11px] text-zinc-600">
              {membersQuery.data?.length ?? 0}
            </span>
            {section === 'members' && (
              <span className="absolute inset-x-0 bottom-0 h-0.5 rounded-full bg-zinc-950" />
            )}
          </button>

          <button
            type="button"
            onClick={() => setSection('invitations')}
            className={cn(
              'relative inline-flex items-center gap-2 px-3 pb-3 text-sm font-medium transition-colors cursor-pointer',
              section === 'invitations' ? 'text-zinc-950' : 'text-zinc-500 hover:text-zinc-900',
            )}
          >
            <Mail className="size-4" />
            邀请记录
            <span
              className={cn(
                'rounded-full px-2 py-0.5 font-mono text-[11px]',
                pendingInvCount > 0 ? 'bg-amber-100 text-amber-800' : 'bg-zinc-100 text-zinc-600',
              )}
            >
              {invitationsQuery.data?.length ?? 0}
            </span>
            {section === 'invitations' && (
              <span className="absolute inset-x-0 bottom-0 h-0.5 rounded-full bg-zinc-950" />
            )}
          </button>
        </div>

        <span className="hidden pb-3 text-xs text-zinc-400 sm:inline">
          你的当前角色：
          <strong className="font-medium text-zinc-700">{ROLE_LABELS[org.role]}</strong>
        </span>
      </div>

      {section === 'members' && (
        <div>
          {membersQuery.isError ? (
            <ErrorAlert
              message={`加载成员失败：${getErrorMessage(membersQuery.error)}`}
              onRetry={() => void membersQuery.refetch()}
            />
          ) : membersQuery.isPending ? (
            <div className="space-y-2.5">
              <Skeleton className="h-16 w-full rounded-xl" />
              <Skeleton className="h-16 w-full rounded-xl" />
            </div>
          ) : (
            <div className="overflow-hidden rounded-xl border border-zinc-200/90 bg-white divide-y divide-zinc-200/80">
              {membersQuery.data.map((member) => {
                const canEditTarget = canManage && canAssignRole(org.role, member.role);
                const displayName = member.name || member.email || `用户 #${member.user_id}`;
                return (
                  <div
                    key={member.user_id}
                    className="flex flex-col gap-3 px-4 py-3.5 transition-colors hover:bg-zinc-50/70 sm:flex-row sm:items-center sm:justify-between"
                  >
                    <div className="flex min-w-0 items-center gap-3">
                      <MonogramAvatar
                        seed={member.email || `u-${member.user_id}`}
                        label={displayName}
                      />
                      <div className="min-w-0">
                        <div className="flex items-center gap-2">
                          <span className="truncate text-sm font-medium text-zinc-950">
                            {displayName}
                          </span>
                          {member.user_id === user.id && (
                            <Badge variant="info" className="px-1.5 py-0 text-[10px]">
                              我
                            </Badge>
                          )}
                        </div>
                        <div className="mt-0.5 flex flex-wrap items-center gap-1.5 text-xs text-zinc-500">
                          <span className="font-mono">
                            {member.email || `ID #${member.user_id}`}
                          </span>
                          <span className="text-zinc-300">·</span>
                          <span title={formatDateTime(member.joined_at)}>
                            加入于 {formatRelativeTime(member.joined_at)}
                          </span>
                        </div>
                      </div>
                    </div>

                    <div className="flex shrink-0 items-center gap-2 sm:justify-end">
                      {canEditTarget ? (
                        <Select
                          aria-label={`修改 ${member.email || member.user_id} 的角色`}
                          className="h-8 text-xs"
                          value={member.role}
                          disabled={updateRole.isPending}
                          onChange={(e) => void handleRoleChange(member, e.target.value as Role)}
                        >
                          {assignableRoles.map((r) => (
                            <option key={r} value={r}>
                              {ROLE_LABELS[r]}
                            </option>
                          ))}
                        </Select>
                      ) : (
                        <Badge variant="outline" className="px-2.5 py-0.5 text-xs">
                          {ROLE_LABELS[member.role]}
                        </Badge>
                      )}

                      {canManage && canEditTarget && (
                        <Button
                          variant="ghost"
                          size="sm"
                          className="h-8 text-zinc-500 hover:bg-red-50 hover:text-destructive"
                          onClick={() => setRemovingMember(member)}
                        >
                          <Trash2 className="size-3.5" />
                          移出
                        </Button>
                      )}
                    </div>
                  </div>
                );
              })}
            </div>
          )}
        </div>
      )}

      {section === 'invitations' && (
        <div>
          {invitationsQuery.isError ? (
            <ErrorAlert
              message={`加载邀请记录失败：${getErrorMessage(invitationsQuery.error)}`}
              onRetry={() => void invitationsQuery.refetch()}
            />
          ) : invitationsQuery.isPending ? (
            <Skeleton className="h-16 w-full rounded-xl" />
          ) : invitationsQuery.data.length === 0 ? (
            <EmptyState
              icon={Mail}
              title="暂无邀请记录"
              description={
                canManage
                  ? '邀请同事后，对方使用同邮箱的 OIDC 或 GitHub 账号登录时会自动加入当前组织。'
                  : '组织管理员发起的成员邀请会展示在这里。'
              }
              action={
                canManage ? (
                  <Button size="sm" onClick={() => setInviteOpen(true)}>
                    <UserPlus className="size-4" />
                    发起第一条邀请
                  </Button>
                ) : undefined
              }
            />
          ) : (
            <div className="overflow-hidden rounded-xl border border-zinc-200/90 bg-white divide-y divide-zinc-200/80">
              {invitationsQuery.data.map((inv) => {
                const statusMeta = INVITATION_STATUS_META[inv.status];
                const canRevoke =
                  canManage && inv.status === 'pending' && canAssignRole(org.role, inv.role);
                return (
                  <div
                    key={inv.id}
                    className="flex flex-col gap-3 px-4 py-3.5 transition-colors hover:bg-zinc-50/70 sm:flex-row sm:items-center sm:justify-between"
                  >
                    <div className="flex min-w-0 items-center gap-3">
                      <MonogramAvatar seed={inv.email} label={inv.email} size="sm" />
                      <div className="min-w-0">
                        <div className="flex flex-wrap items-center gap-2">
                          <span className="truncate font-mono text-sm font-medium text-zinc-950">
                            {inv.email}
                          </span>
                          <Badge variant={statusMeta.variant} className="text-[11px]">
                            {statusMeta.label}
                          </Badge>
                          <Badge variant="outline" className="text-[11px]">
                            {ROLE_LABELS[inv.role]}
                          </Badge>
                        </div>
                        <div className="mt-0.5 flex flex-wrap items-center gap-1.5 text-xs text-zinc-500">
                          <span title={formatDateTime(inv.created_at)}>
                            发起于 {formatRelativeTime(inv.created_at)}
                          </span>
                          {inv.expires_at && (
                            <>
                              <span className="text-zinc-300">·</span>
                              <span>过期 {formatDateTime(inv.expires_at)}</span>
                            </>
                          )}
                        </div>
                      </div>
                    </div>

                    {inv.status === 'pending' && (
                      <div className="flex items-center gap-2 self-start sm:self-center">
                        <CopyButton
                          value={`${window.location.origin}${routes.login()}`}
                          variant="pill"
                          label="复制登录入口"
                          toastMessage="已复制控制台登录入口，可发给受邀同事"
                        />
                        {canRevoke && (
                          <Button
                            variant="ghost"
                            size="sm"
                            className="h-8 text-destructive hover:bg-red-50 hover:text-destructive"
                            onClick={() => setRevokingInv(inv)}
                          >
                            撤销邀请
                          </Button>
                        )}
                      </div>
                    )}
                  </div>
                );
              })}
            </div>
          )}
        </div>
      )}

      <InviteMemberDialog
        orgId={org.id}
        actorRole={org.role}
        open={inviteOpen}
        onOpenChange={setInviteOpen}
      />

      <ConfirmDialog
        open={removingMember !== null}
        onOpenChange={(open) => {
          if (!open) setRemovingMember(null);
        }}
        title={`将 ${removingMember?.email || `用户 #${removingMember?.user_id ?? ''}`} 移出组织？`}
        description="移出后该成员立刻失去本组织的访问权限，但不影响其会话在其他组织的正常使用。"
        confirmLabel="确认移出"
        pending={removeMember.isPending}
        onConfirm={handleConfirmRemove}
      />

      <ConfirmDialog
        open={revokingInv !== null}
        onOpenChange={(open) => {
          if (!open) setRevokingInv(null);
        }}
        title={`撤销对 ${revokingInv?.email ?? ''} 的邀请？`}
        description="撤销后对方登录将不会自动加入本组织；如需重新邀请可再次发起。"
        confirmLabel="确认撤销"
        pending={revokeInvitation.isPending}
        onConfirm={handleConfirmRevoke}
      />
    </div>
  );
}
