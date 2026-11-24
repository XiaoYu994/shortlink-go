import { useMemo, useState } from 'react';
import { Bot, KeyRound, Lock, RefreshCw, ScrollText, Terminal, User } from 'lucide-react';
import { useCurrentOrg } from '@/app/org-context';
import { CopyButton } from '@/components/CopyButton';
import { EmptyState } from '@/components/EmptyState';
import { ErrorAlert } from '@/components/ErrorAlert';
import { PageHeader } from '@/components/PageHeader';
import { Badge, type BadgeProps } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Select } from '@/components/ui/input';
import { Skeleton } from '@/components/ui/skeleton';
import { canReadAuditLogs, ROLE_LABELS } from '@/features/auth/permissions';
import { getErrorMessage } from '@/lib/api-client';
import { cn } from '@/lib/cn';
import { formatDateTime, formatRelativeTime } from '@/lib/format';
import { useDocumentTitle } from '@/lib/use-document-title';
import {
  type AuditAction,
  type AuditActorKind,
  type AuditEvent,
  type AuditTargetType,
  useInfiniteAuditLogs,
} from './api';

const ACTION_META: Record<AuditAction, { label: string; variant: BadgeProps['variant'] }> = {
  'org.create': { label: '创建组织', variant: 'success' },
  'tenant.limits_update': { label: '调整配额限流', variant: 'secondary' },
  'link.create': { label: '创建短链', variant: 'success' },
  'link.update': { label: '修改短链', variant: 'outline' },
  'link.delete': { label: '删除短链', variant: 'destructive' },
  'link.ban': { label: '封禁短链', variant: 'destructive' },
  'link.unban': { label: '解封短链', variant: 'success' },
  'member.role_update': { label: '变更成员角色', variant: 'secondary' },
  'member.remove': { label: '移出成员', variant: 'destructive' },
  'invitation.create': { label: '发起邀请', variant: 'outline' },
  'invitation.revoke': { label: '撤销邀请', variant: 'destructive' },
  'invitation.redeem': { label: '兑现邀请', variant: 'success' },
  'apikey.issue': { label: '签发 API Key', variant: 'success' },
  'apikey.revoke': { label: '吊销 API Key', variant: 'destructive' },
};

const ACTION_OPTIONS: ReadonlyArray<{ value: AuditAction; label: string }> = (
  Object.entries(ACTION_META) as Array<[AuditAction, { label: string }]>
).map(([value, meta]) => ({ value, label: meta.label }));

const TARGET_TYPE_LABELS: Record<AuditTargetType, string> = {
  org: '组织',
  link: '短链',
  member: '成员',
  invitation: '邀请',
  api_key: 'API Key',
};

const METADATA_KEY_LABELS: Record<string, string> = {
  name: '名称',
  email: '邮箱',
  role: '角色',
  previous_role: '原角色',
  new_role: '新角色',
  reason: '原因',
  source: '来源',
  previous_reason: '原封禁原因',
  previous_source: '原封禁来源',
  url: '目标地址',
  disabled: '已停用',
  scopes: '权限范围',
  fields: '变更字段',
  link_quota: '月度配额',
  previous_link_quota: '原配额',
  rate_limit_per_second: '每秒限流',
  previous_rate_limit_per_second: '原限流',
  expires_at: '过期时间',
  clear_expires_at: '清空过期时间',
  external_id: '外部编号',
};

function actorLabel(ev: AuditEvent): string {
  switch (ev.actor_kind) {
    case 'user':
      return ev.actor_name || ev.actor_email || (ev.actor_id ? `用户 #${ev.actor_id}` : '用户');
    case 'api_key':
      return ev.actor_id ? `API Key #${ev.actor_id}` : 'API Key';
    case 'operator':
      return '运维 CLI';
    case 'system':
      return '系统任务';
    default:
      return ev.actor_kind;
  }
}

function ActorIcon({ kind }: { kind: AuditActorKind }) {
  switch (kind) {
    case 'user':
      return <User className="size-4" />;
    case 'api_key':
      return <KeyRound className="size-4" />;
    case 'operator':
      return <Terminal className="size-4" />;
    default:
      return <Bot className="size-4" />;
  }
}

function formatMetadataValue(value: unknown): string {
  if (Array.isArray(value)) {
    return value.map((item) => formatMetadataValue(item)).join(', ');
  }
  if (typeof value === 'boolean') {
    return value ? '是' : '否';
  }
  if (value === null || value === undefined) {
    return '—';
  }
  if (typeof value === 'object') {
    return JSON.stringify(value);
  }
  return String(value);
}

function metadataEntries(
  metadata: Record<string, unknown>,
): Array<{ key: string; label: string; value: string }> {
  return Object.entries(metadata)
    .filter(([k, v]) => k !== 'link_id' && v !== null && v !== undefined && v !== '')
    .map(([key, val]) => ({
      key,
      label: METADATA_KEY_LABELS[key] ?? key,
      value: formatMetadataValue(val),
    }));
}

export function AuditLogsPage() {
  const { org } = useCurrentOrg();
  const allowed = canReadAuditLogs(org.role);

  useDocumentTitle(`审计日志 · ${org.name}`);

  const [actionFilter, setActionFilter] = useState<AuditAction | ''>('');
  const logsQuery = useInfiniteAuditLogs(org.id, actionFilter, allowed);

  const events = useMemo(
    () => logsQuery.data?.pages.flatMap((page) => page.items) ?? [],
    [logsQuery.data],
  );

  if (!allowed) {
    return (
      <div className="space-y-6">
        <PageHeader title="审计日志" description="记录组织内关键资源与安全配置的不可变变更轨迹。" />
        <EmptyState
          icon={Lock}
          title="仅 Owner 与 Admin 可查看审计日志"
          description={`你在「${org.name}」的当前角色是 ${ROLE_LABELS[org.role]}，无权查看组织审计日志。`}
        />
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <PageHeader
        title="审计日志"
        description="与业务写入同事务提交的不可变变更记录，涵盖控制台用户、API Key、运维 CLI 与异步风控。"
        actions={
          <>
            <Select
              aria-label="按操作筛选"
              value={actionFilter}
              onChange={(e) => setActionFilter(e.target.value as AuditAction | '')}
            >
              <option value="">全部操作</option>
              {ACTION_OPTIONS.map((opt) => (
                <option key={opt.value} value={opt.value}>
                  {opt.label}（{opt.value}）
                </option>
              ))}
            </Select>
            <Button
              variant="outline"
              size="sm"
              disabled={logsQuery.isFetching}
              onClick={() => void logsQuery.refetch()}
            >
              <RefreshCw className={cn('size-3.5', logsQuery.isFetching && 'animate-spin')} />
              刷新
            </Button>
          </>
        }
      />

      {logsQuery.isError && (
        <ErrorAlert
          message={`加载审计日志失败：${getErrorMessage(logsQuery.error)}`}
          onRetry={() => void logsQuery.refetch()}
        />
      )}

      {logsQuery.isPending ? (
        <div className="space-y-2.5">
          <Skeleton className="h-[78px] w-full rounded-xl" />
          <Skeleton className="h-[78px] w-full rounded-xl" />
          <Skeleton className="h-[78px] w-full rounded-xl" />
        </div>
      ) : events.length === 0 ? (
        <EmptyState
          icon={ScrollText}
          title={actionFilter ? '当前筛选下没有匹配的审计记录' : '还没有审计日志'}
          description={
            actionFilter
              ? '尝试切换为「全部操作」查看该组织的其他变更记录。'
              : '当成员创建或修改短链、邀请成员、签发密钥或触发风控封禁时，这里会按时间倒序展示轨迹。'
          }
          action={
            actionFilter ? (
              <Button variant="outline" onClick={() => setActionFilter('')}>
                查看全部操作
              </Button>
            ) : undefined
          }
        />
      ) : (
        <>
          <div className="overflow-hidden rounded-xl border border-zinc-200/90 bg-white divide-y divide-zinc-200/80">
            {events.map((ev) => {
              const actionMeta = ACTION_META[ev.action] ?? {
                label: ev.action,
                variant: 'outline' as const,
              };
              const targetLabel = TARGET_TYPE_LABELS[ev.target_type] ?? ev.target_type;
              const details = metadataEntries(ev.metadata);

              return (
                <div
                  key={ev.id}
                  className="flex flex-col gap-2.5 px-4 py-3.5 transition-colors hover:bg-zinc-50/70"
                >
                  <div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
                    <div className="flex min-w-0 items-center gap-3">
                      <span className="flex size-8 shrink-0 items-center justify-center rounded-lg border border-zinc-200 bg-zinc-50 text-zinc-600">
                        <ActorIcon kind={ev.actor_kind} />
                      </span>
                      <div className="flex min-w-0 flex-wrap items-center gap-2">
                        <Badge variant={actionMeta.variant} className="px-2 py-0.5 text-[11px]">
                          {actionMeta.label}
                        </Badge>
                        <span className="truncate text-sm font-medium text-zinc-950">
                          {actorLabel(ev)}
                        </span>
                        {ev.actor_kind === 'user' && ev.actor_email && ev.actor_name && (
                          <span className="hidden truncate text-xs text-zinc-400 sm:inline">
                            {ev.actor_email}
                          </span>
                        )}
                        <span className="text-zinc-300">→</span>
                        <span className="inline-flex items-center gap-1 rounded-md border border-zinc-200/80 bg-zinc-50 px-1.5 py-0.5 text-xs text-zinc-700">
                          <span className="text-zinc-400">{targetLabel}</span>
                          <code className="font-mono font-medium text-zinc-900">
                            {ev.target_id}
                          </code>
                        </span>
                      </div>
                    </div>

                    <div className="flex shrink-0 items-center gap-2 pl-11 sm:pl-0">
                      {ev.request_id && (
                        <CopyButton
                          value={ev.request_id}
                          variant="pill"
                          label={ev.request_id}
                          toastMessage="已复制请求编号"
                          className="h-6 max-w-[180px] truncate font-mono text-[11px] text-zinc-500"
                        />
                      )}
                      <span
                        title={formatDateTime(ev.created_at)}
                        className="text-xs whitespace-nowrap text-zinc-400"
                      >
                        {formatRelativeTime(ev.created_at)}
                      </span>
                    </div>
                  </div>

                  {details.length > 0 && (
                    <div className="flex flex-wrap gap-1.5 pl-11">
                      {details.map((entry) => (
                        <span
                          key={entry.key}
                          className="inline-flex max-w-full items-center gap-1 rounded-md bg-zinc-100/80 px-2 py-0.5 text-[11px] text-zinc-600"
                        >
                          <span className="text-zinc-400">{entry.label}:</span>
                          <span className="truncate font-mono text-zinc-800">{entry.value}</span>
                        </span>
                      ))}
                    </div>
                  )}
                </div>
              );
            })}
          </div>

          {logsQuery.hasNextPage && (
            <div className="flex justify-center pt-1">
              <Button
                variant="outline"
                size="sm"
                disabled={logsQuery.isFetchingNextPage}
                onClick={() => void logsQuery.fetchNextPage()}
              >
                {logsQuery.isFetchingNextPage ? '加载中…' : '加载更多记录'}
              </Button>
            </div>
          )}
        </>
      )}
    </div>
  );
}
