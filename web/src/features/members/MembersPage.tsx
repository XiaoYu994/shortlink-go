import { useState } from 'react';
import { Mail, RefreshCw, Trash2, UserPlus, Users } from 'lucide-react';
import { useSearchParams } from 'react-router';
import { toast } from 'sonner';
import { useCurrentOrg } from '@/app/org-context';
import { routes } from '@/app/routes';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { CopyButton } from '@/components/CopyButton';
import { CourierCat } from '@/components/CourierCat';
import { EmptyState } from '@/components/EmptyState';
import { ErrorAlert } from '@/components/ErrorAlert';
import { PageHeader } from '@/components/PageHeader';
import { StampAvatar } from '@/components/StampAvatar';
import { Badge, type BadgeProps } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Select } from '@/components/ui/input';
import { Skeleton } from '@/components/ui/skeleton';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
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
  pending: { label: '待加入', variant: 'warning' },
  accepted: { label: '已加入', variant: 'success' },
  revoked: { label: '已撤销', variant: 'destructive' },
  expired: { label: '已过期', variant: 'secondary' },
};

export function MembersPage() {
  const { user, org } = useCurrentOrg();
  const [searchParams, setSearchParams] = useSearchParams();
  const canManage = canManageMembers(org.role);
  const assignableRoles = assignableRolesFor(org.role);

  useDocumentTitle(`成员与邀请 · ${org.name}`);

  const section = searchParams.get('tab') === 'invitations' ? 'invitations' : 'members';
  const [inviteOpen, setInviteOpen] = useState(false);
  const [removingMember, setRemovingMember] = useState<Member | null>(null);
  const [revokingInv, setRevokingInv] = useState<Invitation | null>(null);
  const [pendingSelfRole, setPendingSelfRole] = useState<{ member: Member; nextRole: Role } | null>(
    null,
  );

  const membersQuery = useMembers(org.id);
  const invitationsQuery = useInvitations(org.id);
  const updateRole = useUpdateMemberRole(org.id);
  const removeMember = useRemoveMember(org.id);
  const revokeInvitation = useRevokeInvitation(org.id);

  function handleTabChange(nextTab: string) {
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (nextTab === 'invitations') next.set('tab', 'invitations');
        else next.delete('tab');
        return next;
      },
      { replace: true },
    );
  }

  async function commitRoleChange(member: Member, nextRole: Role) {
    try {
      await updateRole.mutateAsync({ userId: member.user_id, role: nextRole });
      toast.success(
        `已将 ${member.email || `用户 #${member.user_id}`} 角色改为 ${ROLE_LABELS[nextRole]}`,
      );
    } catch (err) {
      toast.error(getErrorMessage(err));
    }
  }

  function handleRoleSelect(member: Member, nextRole: Role) {
    if (member.role === nextRole) return;
    // 防自锁保护：管理员调整自己角色前必须经过显式二次确认
    if (member.user_id === user.id) {
      setPendingSelfRole({ member, nextRole });
      return;
    }
    void commitRoleChange(member, nextRole);
  }

  async function handleConfirmSelfRoleChange() {
    if (!pendingSelfRole) return;
    const { member, nextRole } = pendingSelfRole;
    setPendingSelfRole(null);
    await commitRoleChange(member, nextRole);
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
  const ownerCount = membersQuery.data?.filter((item) => item.role === 'owner').length ?? 0;
  const pendingInvCount =
    invitationsQuery.data?.filter((item) => item.status === 'pending').length ?? 0;

  return (
    <div className="space-y-6">
      <PageHeader
        title="成员与邀请"
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
              <Button size="sm" variant="highlight" onClick={() => setInviteOpen(true)}>
                <UserPlus className="size-4" />
                邀请成员
              </Button>
            )}
          </>
        }
      />

      <Tabs value={section} onValueChange={handleTabChange} className="space-y-5">
        <div className="flex items-center justify-between border-b border-border-soft">
          <TabsList className="border-b-0">
            <TabsTrigger value="members">
              <Users className="size-4" />
              组织成员
              <span className="rounded-full border border-ink/30 bg-card px-2 py-0.5 font-mono text-[11px] text-ink">
                {membersQuery.data?.length ?? 0}
              </span>
            </TabsTrigger>

            <TabsTrigger value="invitations">
              <Mail className="size-4" />
              邀请记录
              {pendingInvCount > 0 ? (
                <span className="rounded-full border border-ink bg-hl-peach/75 px-2 py-0.5 font-mono text-[11px] font-bold text-ink">
                  {pendingInvCount} 待加入
                </span>
              ) : (
                <span className="rounded-full border border-ink/25 bg-paper-warm px-2 py-0.5 font-mono text-[11px] text-muted-foreground">
                  {invitationsQuery.data?.length ?? 0}
                </span>
              )}
            </TabsTrigger>
          </TabsList>

          <span className="hidden pb-2 text-xs text-muted-foreground sm:inline">
            你的当前角色：
            <strong className="font-semibold text-ink">{ROLE_LABELS[org.role]}</strong>
          </span>
        </div>

        <TabsContent value="members">
          {membersQuery.isError ? (
            <ErrorAlert
              message={`加载成员失败：${getErrorMessage(membersQuery.error)}`}
              onRetry={() => void membersQuery.refetch()}
            />
          ) : membersQuery.isPending ? (
            <div className="space-y-2.5">
              <Skeleton className="h-16 w-full rounded-2xl" />
              <Skeleton className="h-16 w-full rounded-2xl" />
            </div>
          ) : (
            <div className="divide-y divide-border-soft overflow-hidden rounded-2xl border-[1.25px] border-ink bg-card">
              {membersQuery.data.map((member) => {
                const canEditTarget = canManage && canAssignRole(org.role, member.role);
                const isSoleOwner = member.role === 'owner' && ownerCount <= 1;
                const displayName = member.name || member.email || `用户 #${member.user_id}`;
                const subtitleEmail = member.name && member.email ? member.email : null;
                const roleSelect = (
                  <Select
                    id={`member-role-${member.user_id}`}
                    name="role"
                    aria-label={`修改 ${member.email || member.user_id} 的角色`}
                    className="h-8 text-xs"
                    value={member.role}
                    disabled={updateRole.isPending || isSoleOwner}
                    onChange={(e) => handleRoleSelect(member, e.target.value as Role)}
                  >
                    {assignableRoles.map((r) => (
                      <option key={r} value={r}>
                        {ROLE_LABELS[r]}
                      </option>
                    ))}
                  </Select>
                );
                return (
                  <div
                    key={member.user_id}
                    className="flex flex-col gap-3 px-4 py-3.5 transition-colors hover:bg-paper-warm/45 sm:flex-row sm:items-center sm:justify-between"
                  >
                    <div className="flex min-w-0 items-center gap-3">
                      <StampAvatar
                        seed={member.email || `u-${member.user_id}`}
                        label={displayName}
                      />
                      <div className="min-w-0">
                        <div className="flex items-center gap-2">
                          <span className="truncate text-sm font-semibold text-ink">
                            {displayName}
                          </span>
                          {member.user_id === user.id && (
                            <Badge variant="yellow" className="px-1.5 py-0 text-[10px]">
                              我
                            </Badge>
                          )}
                        </div>
                        <div className="mt-0.5 flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
                          {subtitleEmail && (
                            <>
                              <span className="font-mono text-ink-secondary">{subtitleEmail}</span>
                              <span className="text-ink/30">·</span>
                            </>
                          )}
                          <Tooltip>
                            <TooltipTrigger asChild>
                              <span>加入于 {formatRelativeTime(member.joined_at)}</span>
                            </TooltipTrigger>
                            <TooltipContent>{formatDateTime(member.joined_at)}</TooltipContent>
                          </Tooltip>
                        </div>
                      </div>
                    </div>

                    <div className="flex shrink-0 items-center gap-2 sm:justify-end">
                      {canEditTarget ? (
                        isSoleOwner ? (
                          <Tooltip>
                            <TooltipTrigger asChild>
                              <span className="inline-flex">{roleSelect}</span>
                            </TooltipTrigger>
                            <TooltipContent>组织至少需保留一位所有者</TooltipContent>
                          </Tooltip>
                        ) : (
                          roleSelect
                        )
                      ) : (
                        <Badge variant="outline" className="px-2.5 py-0.5 text-xs">
                          {ROLE_LABELS[member.role]}
                        </Badge>
                      )}

                      {canManage && canEditTarget && member.user_id !== user.id && (
                        <Button
                          variant="ghost"
                          size="sm"
                          className="h-8 text-ink-secondary hover:bg-red-50 hover:text-stamp-red"
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
        </TabsContent>

        <TabsContent value="invitations">
          {invitationsQuery.isError ? (
            <ErrorAlert
              message={`加载邀请记录失败：${getErrorMessage(invitationsQuery.error)}`}
              onRetry={() => void invitationsQuery.refetch()}
            />
          ) : invitationsQuery.isPending ? (
            <Skeleton className="h-20 w-full rounded-2xl" />
          ) : invitationsQuery.data.length === 0 ? (
            <EmptyState
              icon={Mail}
              illustration={<CourierCat pose="empty" />}
              title="暂无邀请记录"
              description={
                canManage
                  ? '登记受邀人邮箱与角色后，对方使用该邮箱登录即可自动加入当前组织；如已开通邮件通知，系统也会同步发送邀请邮件。'
                  : '组织管理员发起的成员邀请会展示在这里。'
              }
              action={
                canManage ? (
                  <Button size="sm" variant="highlight" onClick={() => setInviteOpen(true)}>
                    <UserPlus className="size-4" />
                    发起第一条邀请
                  </Button>
                ) : undefined
              }
            />
          ) : (
            <div className="divide-y divide-border-soft overflow-hidden rounded-2xl border-[1.25px] border-ink bg-card">
              {invitationsQuery.data.map((inv) => {
                const statusMeta = INVITATION_STATUS_META[inv.status];
                const canRevoke =
                  canManage && inv.status === 'pending' && canAssignRole(org.role, inv.role);
                return (
                  <div
                    key={inv.id}
                    className="flex flex-col gap-3 px-4 py-3.5 transition-colors hover:bg-paper-warm/45 sm:flex-row sm:items-center sm:justify-between"
                  >
                    <div className="flex min-w-0 items-center gap-3">
                      <StampAvatar seed={inv.email} label={inv.email} size="sm" />
                      <div className="min-w-0">
                        <div className="flex flex-wrap items-center gap-2">
                          <span className="truncate font-mono text-sm font-semibold text-ink">
                            {inv.email}
                          </span>
                          <Badge variant={statusMeta.variant} className="text-[11px]">
                            {statusMeta.label}
                          </Badge>
                          <Badge variant="outline" className="text-[11px]">
                            {ROLE_LABELS[inv.role]}
                          </Badge>
                        </div>
                        <div className="mt-0.5 flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
                          <Tooltip>
                            <TooltipTrigger asChild>
                              <span>发起于 {formatRelativeTime(inv.created_at)}</span>
                            </TooltipTrigger>
                            <TooltipContent>{formatDateTime(inv.created_at)}</TooltipContent>
                          </Tooltip>
                          {inv.expires_at && (
                            <>
                              <span className="text-ink/30">·</span>
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
                          toastMessage="已复制工作台登录入口，可发给受邀成员"
                        />
                        {canRevoke && (
                          <Button
                            variant="ghost"
                            size="sm"
                            className="h-8 text-stamp-red hover:bg-red-50 hover:text-stamp-red"
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
        </TabsContent>
      </Tabs>

      <InviteMemberDialog
        orgId={org.id}
        actorRole={org.role}
        open={inviteOpen}
        onOpenChange={setInviteOpen}
      />

      <ConfirmDialog
        open={pendingSelfRole !== null}
        onOpenChange={(open) => {
          if (!open) setPendingSelfRole(null);
        }}
        title={`确认将自己的角色改为 ${pendingSelfRole ? ROLE_LABELS[pendingSelfRole.nextRole] : ''}？`}
        description="调整为低权限角色后，你将立即失去成员管理、API Key 与审计日志权限，且无法自行恢复。"
        confirmLabel="确认变更"
        pending={updateRole.isPending}
        onConfirm={handleConfirmSelfRoleChange}
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
