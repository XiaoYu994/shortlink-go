import { useMemo, useState } from 'react';
import { KeyRound, Lock, Plus, RefreshCw, Terminal, Trash2 } from 'lucide-react';
import { toast } from 'sonner';
import { useCurrentOrg } from '@/app/org-context';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { CopyButton } from '@/components/CopyButton';
import { CourierCat } from '@/components/CourierCat';
import { EmptyState } from '@/components/EmptyState';
import { ErrorAlert } from '@/components/ErrorAlert';
import { OneTimeSecretBanner } from '@/components/OneTimeSecretBanner';
import { PageHeader } from '@/components/PageHeader';
import { Badge, type BadgeProps } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Skeleton } from '@/components/ui/skeleton';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import { canManageAPIKeys, ROLE_LABELS } from '@/features/auth/permissions';
import { useMembers } from '@/features/members/api';
import { getErrorMessage } from '@/lib/api-client';
import { cn } from '@/lib/cn';
import { ENDPOINTS } from '@/lib/endpoints';
import { formatDateTime, formatRelativeTime } from '@/lib/format';
import { useDocumentTitle } from '@/lib/use-document-title';
import {
  type APIKeyInfo,
  type APIKeyScope,
  type APIKeyStatus,
  type IssuedAPIKey,
  useAPIKeys,
  useRevokeAPIKey,
} from './api';
import { IssueAPIKeyDialog } from './IssueAPIKeyDialog';

const KEY_STATUS_META: Record<
  APIKeyStatus,
  { label: string; variant: BadgeProps['variant']; dot: string }
> = {
  active: { label: '有效', variant: 'success', dot: 'bg-emerald-600' },
  revoked: { label: '已吊销', variant: 'destructive', dot: 'bg-stamp-red' },
  expired: { label: '已过期', variant: 'secondary', dot: 'bg-ink-muted' },
};

type KeyFilter = 'all' | 'active' | 'inactive';

function buildCurlExample(scopes: APIKeyScope[], tokenExpr: string): string {
  const origin = window.location.origin;
  if (scopes.includes('links:write')) {
    return `curl -sS -X POST ${origin}${ENDPOINTS.links.listCreate} \\\n  -H 'Authorization: Bearer ${tokenExpr}' \\\n  -H 'Content-Type: application/json' \\\n  -d '{"url":"https://example.com"}'`;
  }
  if (scopes.includes('links:read')) {
    return `curl -sS '${origin}${ENDPOINTS.links.listCreate}?limit=10' \\\n  -H 'Authorization: Bearer ${tokenExpr}'`;
  }
  return `curl -sS '${origin}${ENDPOINTS.links.stats('<短码>')}' \\\n  -H 'Authorization: Bearer ${tokenExpr}'`;
}

export function APIKeysPage() {
  const { org } = useCurrentOrg();
  const allowed = canManageAPIKeys(org.role);

  useDocumentTitle(`API Keys · ${org.name}`);

  const [filter, setFilter] = useState<KeyFilter>('all');
  const [issueOpen, setIssueOpen] = useState(false);
  const [issuedSecret, setIssuedSecret] = useState<{ orgId: number; key: IssuedAPIKey } | null>(
    null,
  );
  const [embedPlaintextInCurl, setEmbedPlaintextInCurl] = useState(false);
  const [revokingKey, setRevokingKey] = useState<APIKeyInfo | null>(null);
  const visibleSecret = issuedSecret && issuedSecret.orgId === org.id ? issuedSecret.key : null;

  const keysQuery = useAPIKeys(org.id, allowed);
  // 复用成员列表，把 created_by（数字 ID）翻译为真实成员姓名或邮箱；未命中时安全回退为 user #id。
  const membersQuery = useMembers(org.id, allowed);
  const memberNameById = useMemo(() => {
    const map = new Map<number, string>();
    for (const m of membersQuery.data ?? []) {
      map.set(m.user_id, m.name || m.email || `用户 #${m.user_id}`);
    }
    return map;
  }, [membersQuery.data]);
  const revokeKey = useRevokeAPIKey(org.id);

  const filteredKeys = useMemo(() => {
    const list = keysQuery.data ?? [];
    if (filter === 'active') return list.filter((k) => k.status === 'active');
    if (filter === 'inactive') return list.filter((k) => k.status !== 'active');
    return list;
  }, [keysQuery.data, filter]);

  async function handleConfirmRevoke() {
    if (!revokingKey) return;
    try {
      await revokeKey.mutateAsync(revokingKey.id);
      toast.success(`已吊销 API Key #${revokingKey.id}`);
      setRevokingKey(null);
    } catch (err) {
      toast.error(getErrorMessage(err));
    }
  }

  if (!allowed) {
    return (
      <div className="space-y-6">
        <PageHeader title="API Keys" />
        <EmptyState
          icon={Lock}
          illustration={<CourierCat pose="shield" />}
          title="仅所有者与管理员可管理 API Key"
          description={`你在「${org.name}」的当前角色是 ${ROLE_LABELS[org.role]}，无权查看、创建或吊销组织密钥。`}
        />
      </div>
    );
  }

  const curlSnippet = visibleSecret
    ? buildCurlExample(
        visibleSecret.scopes,
        embedPlaintextInCurl ? visibleSecret.api_key : '$ORI_API_KEY',
      )
    : '';

  return (
    <div className="space-y-6">
      <PageHeader
        title="API Keys"
        actions={
          <>
            <Button
              variant="outline"
              size="sm"
              disabled={keysQuery.isFetching}
              onClick={() => void keysQuery.refetch()}
            >
              <RefreshCw className={cn('size-3.5', keysQuery.isFetching && 'animate-spin')} />
              刷新
            </Button>
            <Button size="sm" variant="highlight" onClick={() => setIssueOpen(true)}>
              <Plus className="size-4" />
              创建 API Key
            </Button>
          </>
        }
      />

      {visibleSecret && (
        <OneTimeSecretBanner
          title={
            visibleSecret.name
              ? `API Key「${visibleSecret.name}」（#${visibleSecret.id}）已创建`
              : `API Key #${visibleSecret.id} 已创建`
          }
          description="密钥明文仅在创建时完整展示一次，关闭后将无法再次查看，请妥善保存。"
          secret={visibleSecret.api_key}
          secretTestId="issued-api-key-secret"
          copyLabel="复制密钥"
          copyToastMessage="已复制明文 API Key"
          onDismiss={() => {
            setIssuedSecret(null);
            setEmbedPlaintextInCurl(false);
          }}
        >
          <div className="overflow-hidden rounded-xl border-[1.25px] border-ink bg-ink text-primary-foreground">
            <div className="flex flex-wrap items-center justify-between gap-2 border-b border-white/15 px-3 py-2 text-[11px] text-white/80">
              <span className="inline-flex items-center gap-1.5 font-mono">
                <Terminal className="size-3.5" />
                按所选权限生成的快速验证命令
              </span>
              <div className="flex items-center gap-3">
                <label className="inline-flex items-center gap-1.5 text-[11px] text-white/85 cursor-pointer select-none">
                  <input
                    type="checkbox"
                    className="size-3.5 rounded accent-hl-yellow"
                    checked={embedPlaintextInCurl}
                    onChange={(e) => setEmbedPlaintextInCurl(e.target.checked)}
                  />
                  在示例命令中填入本次明文
                </label>
                <CopyButton
                  value={curlSnippet}
                  variant="pill"
                  label="复制命令"
                  toastMessage="已复制示例命令"
                  className="h-6 border-white/30 bg-white/10 text-white hover:bg-white/20 hover:text-white"
                />
              </div>
            </div>
            <pre
              data-testid="issued-api-key-curl"
              className="overflow-x-auto p-3 font-mono text-xs leading-relaxed text-amber-100"
            >
              {curlSnippet}
            </pre>
          </div>
        </OneTimeSecretBanner>
      )}

      {keysQuery.data && keysQuery.data.length > 0 && (
        <div role="group" aria-label="密钥状态筛选" className="flex items-center gap-1.5">
          {(
            [
              { id: 'all', label: '全部' },
              { id: 'active', label: '有效' },
              { id: 'inactive', label: '已吊销 / 过期' },
            ] as const
          ).map((item) => (
            <button
              key={item.id}
              type="button"
              aria-pressed={filter === item.id}
              onClick={() => setFilter(item.id)}
              className={cn(
                'rounded-lg px-2.5 py-1.5 text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink cursor-pointer',
                filter === item.id
                  ? 'hl-swipe-mint font-bold text-ink'
                  : 'text-ink-secondary hover:bg-paper-warm hover:text-ink',
              )}
            >
              {item.label}
            </button>
          ))}
        </div>
      )}

      {keysQuery.isError && (
        <ErrorAlert
          message={`加载 API Key 失败：${getErrorMessage(keysQuery.error)}`}
          onRetry={() => void keysQuery.refetch()}
        />
      )}

      {keysQuery.isPending ? (
        <div className="space-y-2.5">
          <Skeleton className="h-[76px] w-full rounded-2xl" />
          <Skeleton className="h-[76px] w-full rounded-2xl" />
        </div>
      ) : keysQuery.data && keysQuery.data.length === 0 ? (
        <EmptyState
          icon={KeyRound}
          illustration={<CourierCat pose="empty" />}
          title="还没有组织 API Key"
          description="创建一把按需授权的密钥，供外部程序或自动化脚本调用。"
          action={
            <Button variant="highlight" onClick={() => setIssueOpen(true)}>
              <Plus className="size-4" />
              创建第一把 API Key
            </Button>
          }
        />
      ) : filteredKeys.length === 0 ? (
        <EmptyState
          icon={KeyRound}
          title="当前状态筛选下没有密钥"
          description="尝试切换为「全部」查看历史吊销或过期的 API Key。"
          action={
            <Button variant="outline" size="sm" onClick={() => setFilter('all')}>
              查看全部密钥
            </Button>
          }
        />
      ) : (
        <div className="divide-y divide-border-soft overflow-hidden rounded-2xl border-[1.25px] border-ink bg-card">
          {filteredKeys.map((key) => {
            const statusMeta = KEY_STATUS_META[key.status];
            const inactive = key.status !== 'active';
            return (
              <div
                key={key.id}
                className={cn(
                  'flex flex-col gap-3 px-4 py-3.5 transition-colors hover:bg-paper-warm/45 sm:flex-row sm:items-center sm:justify-between',
                  inactive && 'bg-paper-warm/35 opacity-70',
                )}
              >
                <div className="flex min-w-0 items-start gap-3">
                  <span className="mt-0.5 flex size-9 shrink-0 items-center justify-center rounded-xl border-[1.25px] border-ink bg-paper-warm text-ink">
                    <KeyRound className="size-4" />
                  </span>
                  <div className="min-w-0 space-y-1.5">
                    <div className="flex flex-wrap items-center gap-2">
                      {key.name ? (
                        <>
                          <span className="text-sm font-bold text-ink">{key.name}</span>
                          <span className="font-mono text-xs text-muted-foreground">#{key.id}</span>
                        </>
                      ) : (
                        <span className="font-mono text-sm font-bold text-ink">#{key.id}</span>
                      )}
                      <Badge
                        variant={statusMeta.variant}
                        className="gap-1.5 px-2 py-0.5 text-[11px]"
                      >
                        <span className={`size-1.5 rounded-full ${statusMeta.dot}`} />
                        {statusMeta.label}
                      </Badge>
                      <span className="text-xs text-muted-foreground">
                        {key.created_by
                          ? `由 ${memberNameById.get(key.created_by) ?? `用户 #${key.created_by}`} 创建`
                          : '由平台管理员创建'}
                      </span>
                    </div>

                    <div className="flex flex-wrap items-center gap-1.5">
                      {key.scopes.map((scope) => (
                        <span
                          key={scope}
                          className="rounded-md border border-ink/35 bg-paper-warm px-1.5 py-0.5 font-mono text-[11px] font-medium text-ink"
                        >
                          {scope}
                        </span>
                      ))}
                      <span className="text-ink/30">·</span>
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <span className="text-xs text-muted-foreground">
                            创建于 {formatRelativeTime(key.created_at)}
                          </span>
                        </TooltipTrigger>
                        <TooltipContent>{formatDateTime(key.created_at)}</TooltipContent>
                      </Tooltip>
                      <span className="text-ink/30">·</span>
                      {key.last_used_at ? (
                        <Tooltip>
                          <TooltipTrigger asChild>
                            <span className="text-xs font-medium text-ink-secondary">
                              最近使用 {formatRelativeTime(key.last_used_at)}
                            </span>
                          </TooltipTrigger>
                          <TooltipContent>{formatDateTime(key.last_used_at)}</TooltipContent>
                        </Tooltip>
                      ) : (
                        <span className="text-xs text-muted-foreground">尚未使用</span>
                      )}
                      {key.expires_at && (
                        <>
                          <span className="text-ink/30">·</span>
                          <span className="text-xs text-muted-foreground">
                            过期 {formatDateTime(key.expires_at)}
                          </span>
                        </>
                      )}
                      {key.revoked_at && (
                        <>
                          <span className="text-ink/30">·</span>
                          <span className="text-xs font-medium text-stamp-red">
                            吊销于 {formatDateTime(key.revoked_at)}
                          </span>
                        </>
                      )}
                    </div>
                  </div>
                </div>

                {key.status === 'active' && (
                  <Button
                    variant="ghost"
                    size="sm"
                    className="h-8 self-start text-ink-secondary hover:bg-red-50 hover:text-stamp-red sm:self-center"
                    onClick={() => setRevokingKey(key)}
                  >
                    <Trash2 className="size-3.5" />
                    吊销
                  </Button>
                )}
              </div>
            );
          })}
        </div>
      )}

      <IssueAPIKeyDialog
        orgId={org.id}
        open={issueOpen}
        onOpenChange={setIssueOpen}
        onIssued={(issued) => {
          setEmbedPlaintextInCurl(false);
          setIssuedSecret({ orgId: org.id, key: issued });
        }}
      />

      <ConfirmDialog
        open={revokingKey !== null}
        onOpenChange={(open) => {
          if (!open) setRevokingKey(null);
        }}
        title={
          revokingKey?.name
            ? `吊销 API Key「${revokingKey.name}」（#${revokingKey.id}）？`
            : `吊销 API Key #${revokingKey?.id ?? ''}？`
        }
        description="吊销后使用该密钥的自动化任务或脚本将立即失效且无法恢复。"
        confirmLabel="确认吊销"
        pending={revokeKey.isPending}
        onConfirm={handleConfirmRevoke}
      />
    </div>
  );
}
