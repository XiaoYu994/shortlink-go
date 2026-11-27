import { useMemo } from 'react';
import {
  ArrowRight,
  Bot,
  KeyRound,
  Lock,
  RefreshCw,
  ScrollText,
  Terminal,
  User,
} from 'lucide-react';
import { Link, useSearchParams } from 'react-router';
import { useCurrentOrg } from '@/app/org-context';
import { routes } from '@/app/routes';
import { CopyButton } from '@/components/CopyButton';
import { CourierCat } from '@/components/CourierCat';
import { EmptyState } from '@/components/EmptyState';
import { ErrorAlert } from '@/components/ErrorAlert';
import { PageHeader } from '@/components/PageHeader';
import { Badge, type BadgeProps } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Select } from '@/components/ui/input';
import { Skeleton } from '@/components/ui/skeleton';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import { type Role, useWebhooksEnabled } from '@/features/auth/api';
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
  'invitation.redeem': { label: '接受邀请', variant: 'success' },
  'apikey.issue': { label: '创建 API Key', variant: 'success' },
  'apikey.revoke': { label: '吊销 API Key', variant: 'destructive' },
  'webhook.create': { label: '创建 Webhook', variant: 'success' },
  'webhook.update': { label: '更新 Webhook', variant: 'outline' },
  'webhook.delete': { label: '删除 Webhook', variant: 'destructive' },
  'webhook.rotate_secret': { label: '更换 Webhook 密钥', variant: 'secondary' },
  'webhook.test': { label: '发送 Webhook 测试', variant: 'outline' },
  'webhook.auto_disable': { label: '自动暂停 Webhook', variant: 'destructive' },
};

const ACTION_GROUPS: ReadonlyArray<{ label: string; actions: AuditAction[] }> = [
  {
    label: '短链管理',
    actions: ['link.create', 'link.update', 'link.delete', 'link.ban', 'link.unban'],
  },
  {
    label: '成员与邀请',
    actions: [
      'member.role_update',
      'member.remove',
      'invitation.create',
      'invitation.revoke',
      'invitation.redeem',
    ],
  },
  {
    label: 'API Keys',
    actions: ['apikey.issue', 'apikey.revoke'],
  },
  {
    label: 'Webhooks',
    actions: [
      'webhook.create',
      'webhook.update',
      'webhook.delete',
      'webhook.rotate_secret',
      'webhook.test',
      'webhook.auto_disable',
    ],
  },
  {
    label: '组织与配额',
    actions: ['org.create', 'tenant.limits_update'],
  },
];

const TARGET_TYPE_LABELS: Record<AuditTargetType, string> = {
  org: '组织',
  link: '短链',
  member: '成员',
  invitation: '邀请',
  api_key: 'API Key',
  webhook: 'Webhook',
};

const METADATA_KEY_LABELS: Record<string, string> = {
  name: '名称',
  email: '邮箱',
  role: '角色',
  reason: '原因',
  source: '来源',
  previous_reason: '原封禁原因',
  previous_source: '原封禁来源',
  url: '目标地址',
  disabled: '已停用',
  enabled: '已启用',
  scopes: '权限范围',
  fields: '变更字段',
  events: '订阅事件',
  secret_prefix: '密钥前缀',
  description: '备注',
  delivery_id: '投递编号',
  job_id: '任务编号',
  consecutive_failures: '连续失败次数',
  disabled_reason: '停用原因',
  expires_at: '过期时间',
  clear_expires_at: '清空过期时间',
  external_id: '外部编号',
};

const CONSUMED_DIFF_KEYS = new Set([
  'link_id',
  'previous_role',
  'new_role',
  'previous_link_quota',
  'link_quota',
  'previous_rate_limit_per_second',
  'rate_limit_per_second',
]);

function actorLabel(ev: AuditEvent): string {
  switch (ev.actor_kind) {
    case 'user':
      return ev.actor_name || ev.actor_email || (ev.actor_id ? `用户 #${ev.actor_id}` : '用户');
    case 'api_key':
      return ev.actor_id ? `API Key #${ev.actor_id}` : 'API Key';
    case 'operator':
      return '平台管理员';
    case 'system':
      return '安全系统';
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

function formatRoleValue(val: unknown): string {
  if (typeof val === 'string' && Object.hasOwn(ROLE_LABELS, val)) {
    return ROLE_LABELS[val as Role];
  }
  return formatMetadataValue(val);
}

function formatClockTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return formatRelativeTime(iso);
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
}

interface MetadataChip {
  key: string;
  label: string;
  value: string;
  isDiff?: boolean;
}

function metadataEntries(metadata: Record<string, unknown>): MetadataChip[] {
  const chips: MetadataChip[] = [];

  if (metadata.previous_role !== undefined && metadata.new_role !== undefined) {
    chips.push({
      key: 'diff-role',
      label: '角色变更',
      value: `${formatRoleValue(metadata.previous_role)} → ${formatRoleValue(metadata.new_role)}`,
      isDiff: true,
    });
  }

  if (metadata.link_quota !== undefined) {
    const prev = metadata.previous_link_quota;
    chips.push({
      key: 'diff-quota',
      label: '月度配额',
      value:
        prev !== undefined
          ? `${formatMetadataValue(prev)} → ${formatMetadataValue(metadata.link_quota)}`
          : formatMetadataValue(metadata.link_quota),
      isDiff: prev !== undefined,
    });
  }

  if (metadata.rate_limit_per_second !== undefined) {
    const prev = metadata.previous_rate_limit_per_second;
    chips.push({
      key: 'diff-rate',
      label: '每秒限流',
      value:
        prev !== undefined
          ? `${formatMetadataValue(prev)} → ${formatMetadataValue(metadata.rate_limit_per_second)}`
          : formatMetadataValue(metadata.rate_limit_per_second),
      isDiff: prev !== undefined,
    });
  }

  for (const [key, val] of Object.entries(metadata)) {
    if (CONSUMED_DIFF_KEYS.has(key) || val === null || val === undefined || val === '') continue;
    chips.push({
      key,
      label: METADATA_KEY_LABELS[key] ?? key,
      value: formatMetadataValue(val),
    });
  }

  return chips;
}

function dateGroupLabel(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '历史记录';
  const now = new Date();
  const sameDay = (a: Date, b: Date) =>
    a.getFullYear() === b.getFullYear() &&
    a.getMonth() === b.getMonth() &&
    a.getDate() === b.getDate();
  if (sameDay(d, now)) return '今天';
  const yesterday = new Date(now);
  yesterday.setDate(now.getDate() - 1);
  if (sameDay(d, yesterday)) return '昨天';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

function isAuditAction(raw: string | null): raw is AuditAction {
  return Boolean(raw && Object.hasOwn(ACTION_META, raw));
}

export function AuditLogsPage() {
  const { org } = useCurrentOrg();
  const [searchParams, setSearchParams] = useSearchParams();
  const allowed = canReadAuditLogs(org.role);
  const webhooksEnabled = useWebhooksEnabled(allowed);
  const visibleActionGroups = useMemo(
    () => (webhooksEnabled ? ACTION_GROUPS : ACTION_GROUPS.filter((g) => g.label !== 'Webhooks')),
    [webhooksEnabled],
  );

  useDocumentTitle(`审计日志 · ${org.name}`);

  const rawAction = searchParams.get('action');
  const actionFilter: AuditAction | '' = isAuditAction(rawAction) ? rawAction : '';
  const logsQuery = useInfiniteAuditLogs(org.id, actionFilter, allowed);

  const events = useMemo(
    () => logsQuery.data?.pages.flatMap((page) => page.items) ?? [],
    [logsQuery.data],
  );

  const groupedEvents = useMemo(() => {
    const groups: Array<{ dateLabel: string; items: AuditEvent[] }> = [];
    for (const ev of events) {
      const label = dateGroupLabel(ev.created_at);
      const last = groups[groups.length - 1];
      if (!last || last.dateLabel !== label) {
        groups.push({ dateLabel: label, items: [ev] });
      } else {
        last.items.push(ev);
      }
    }
    return groups;
  }, [events]);

  function handleActionFilterChange(nextAction: AuditAction | '') {
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (nextAction) next.set('action', nextAction);
        else next.delete('action');
        return next;
      },
      { replace: true },
    );
  }

  if (!allowed) {
    return (
      <div className="space-y-6">
        <PageHeader title="审计日志" />
        <EmptyState
          icon={Lock}
          illustration={<CourierCat pose="shield" />}
          title="仅所有者与管理员可查看审计日志"
          description={`你在「${org.name}」的当前角色是 ${ROLE_LABELS[org.role]}，无权查看组织审计日志。`}
        />
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <PageHeader
        title="审计日志"
        actions={
          <Button
            variant="outline"
            size="sm"
            disabled={logsQuery.isFetching}
            onClick={() => void logsQuery.refetch()}
          >
            <RefreshCw className={cn('size-3.5', logsQuery.isFetching && 'animate-spin')} />
            刷新
          </Button>
        }
      />

      {/* 筛选栏下沉正文顶部并按领域分组 */}
      <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-border-soft bg-paper-warm/50 px-3.5 py-2.5">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-xs font-medium text-ink-secondary">筛选操作类型</span>
          <Select
            id="audit-action-filter"
            name="action"
            aria-label="按操作筛选"
            value={actionFilter}
            onChange={(e) => handleActionFilterChange(e.target.value as AuditAction | '')}
          >
            <option value="">全部操作</option>
            {visibleActionGroups.map((group) => (
              <optgroup key={group.label} label={group.label}>
                {group.actions.map((act) => (
                  <option key={act} value={act}>
                    {ACTION_META[act].label}
                  </option>
                ))}
              </optgroup>
            ))}
          </Select>
          {actionFilter && (
            <Button variant="ghost" size="sm" onClick={() => handleActionFilterChange('')}>
              重置
            </Button>
          )}
        </div>

        <span className="text-xs text-muted-foreground">已加载 {events.length} 条记录</span>
      </div>

      {logsQuery.isError && (
        <ErrorAlert
          message={`加载审计日志失败：${getErrorMessage(logsQuery.error)}`}
          onRetry={() => void logsQuery.refetch()}
        />
      )}

      {logsQuery.isPending ? (
        <div className="space-y-2.5">
          <Skeleton className="h-[78px] w-full rounded-2xl" />
          <Skeleton className="h-[78px] w-full rounded-2xl" />
          <Skeleton className="h-[78px] w-full rounded-2xl" />
        </div>
      ) : events.length === 0 ? (
        <EmptyState
          icon={ScrollText}
          illustration={<CourierCat pose="empty" />}
          title={actionFilter ? '当前筛选下没有匹配的审计记录' : '还没有审计日志'}
          description={
            actionFilter
              ? '尝试切换为「全部操作」查看该组织的其他变更记录。'
              : '当成员创建或修改短链、邀请成员、创建密钥或触发风控封禁时，这里会按时间倒序展示轨迹。'
          }
          action={
            actionFilter ? (
              <Button variant="outline" onClick={() => handleActionFilterChange('')}>
                查看全部操作
              </Button>
            ) : undefined
          }
        />
      ) : (
        <div className="space-y-5">
          {groupedEvents.map((group) => (
            <section key={group.dateLabel} className="space-y-2">
              <div className="flex items-center gap-2 px-1">
                <span className="hl-swipe-yellow px-2 py-0.5 text-xs font-bold text-ink">
                  {group.dateLabel}
                </span>
                <span className="h-px flex-1 border-t border-dashed border-ink/25" />
              </div>

              <div className="divide-y divide-border-soft overflow-hidden rounded-2xl border-[1.25px] border-ink bg-card">
                {group.items.map((ev) => {
                  const actionMeta = ACTION_META[ev.action] ?? {
                    label: ev.action,
                    variant: 'outline' as const,
                  };
                  const targetLabel = TARGET_TYPE_LABELS[ev.target_type] ?? ev.target_type;
                  const details = metadataEntries(ev.metadata);
                  const linkTargetReachable =
                    ev.target_type === 'link' && ev.action !== 'link.delete';

                  return (
                    <div
                      key={ev.id}
                      className="group flex flex-col gap-2.5 px-4 py-3.5 transition-colors hover:bg-paper-warm/45"
                    >
                      <div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
                        <div className="flex min-w-0 items-center gap-3">
                          <span className="flex size-8 shrink-0 items-center justify-center rounded-lg border-[1.25px] border-ink/80 bg-paper-warm text-ink">
                            <ActorIcon kind={ev.actor_kind} />
                          </span>
                          <div className="flex min-w-0 flex-wrap items-center gap-2">
                            <Badge variant={actionMeta.variant} className="px-2 py-0.5 text-[11px]">
                              {actionMeta.label}
                            </Badge>
                            <span className="truncate text-sm font-semibold text-ink">
                              {actorLabel(ev)}
                            </span>
                            {ev.actor_kind === 'user' && ev.actor_email && ev.actor_name && (
                              <span className="hidden truncate text-xs text-muted-foreground sm:inline">
                                {ev.actor_email}
                              </span>
                            )}
                            <ArrowRight
                              className="size-3.5 shrink-0 text-ink/40"
                              aria-hidden="true"
                            />
                            <span className="inline-flex items-center gap-1 rounded-md border border-ink/35 bg-paper-warm px-1.5 py-0.5 text-xs text-ink">
                              <span className="text-muted-foreground">{targetLabel}</span>
                              {linkTargetReachable ? (
                                <Link
                                  to={routes.linkStats(org.id, ev.target_id)}
                                  className="font-mono font-bold text-ink underline decoration-hl-yellow decoration-2 underline-offset-2 hover:bg-hl-yellow/40"
                                >
                                  {ev.target_id}
                                </Link>
                              ) : (
                                <code className="font-mono font-semibold text-ink">
                                  {ev.target_id}
                                </code>
                              )}
                            </span>
                          </div>
                        </div>

                        <div className="flex shrink-0 items-center gap-1.5 pl-11 sm:pl-0">
                          {ev.request_id && (
                            <Tooltip>
                              <TooltipTrigger asChild>
                                <span className="inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 font-mono text-[11px] text-muted-foreground opacity-80 transition-opacity group-hover:opacity-100">
                                  <span className="hidden max-w-[110px] truncate md:inline">
                                    {ev.request_id}
                                  </span>
                                  <span className="font-sans md:hidden">请求编号</span>
                                  <CopyButton
                                    value={ev.request_id}
                                    label={`复制请求编号 ${ev.request_id}`}
                                    toastMessage="已复制请求编号"
                                  />
                                </span>
                              </TooltipTrigger>
                              <TooltipContent>请求编号：{ev.request_id}</TooltipContent>
                            </Tooltip>
                          )}
                          <Tooltip>
                            <TooltipTrigger asChild>
                              <span className="font-mono text-xs whitespace-nowrap text-muted-foreground tabular-nums">
                                {formatClockTime(ev.created_at)}
                              </span>
                            </TooltipTrigger>
                            <TooltipContent>{formatDateTime(ev.created_at)}</TooltipContent>
                          </Tooltip>
                        </div>
                      </div>

                      {details.length > 0 && (
                        <div className="flex flex-wrap gap-1.5 pl-11">
                          {details.map((entry) => (
                            <span
                              key={entry.key}
                              title={`${entry.label}: ${entry.value}`}
                              className={cn(
                                'inline-flex max-w-full items-center gap-1 rounded-md px-2 py-0.5 text-[11px]',
                                entry.isDiff
                                  ? 'border border-ink/40 bg-hl-yellow/45 font-medium text-ink'
                                  : 'bg-paper-warm text-ink-secondary',
                              )}
                            >
                              <span className="text-muted-foreground">{entry.label}:</span>
                              <span className="truncate font-mono text-ink">{entry.value}</span>
                            </span>
                          ))}
                        </div>
                      )}
                    </div>
                  );
                })}
              </div>
            </section>
          ))}

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
        </div>
      )}
    </div>
  );
}
