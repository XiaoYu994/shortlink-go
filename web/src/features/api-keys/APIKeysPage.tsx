import { useMemo, useState } from 'react';
import { KeyRound, Lock, Plus, RefreshCw, ShieldAlert, Terminal, Trash2 } from 'lucide-react';
import { toast } from 'sonner';
import { useCurrentOrg } from '@/app/org-context';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { CopyButton } from '@/components/CopyButton';
import { EmptyState } from '@/components/EmptyState';
import { ErrorAlert } from '@/components/ErrorAlert';
import { PageHeader } from '@/components/PageHeader';
import { Badge, type BadgeProps } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { canManageAPIKeys, ROLE_LABELS } from '@/features/auth/permissions';
import { useMembers } from '@/features/members/api';
import { getErrorMessage } from '@/lib/api-client';
import { cn } from '@/lib/cn';
import { ENDPOINTS } from '@/lib/endpoints';
import { formatDateTime, formatRelativeTime } from '@/lib/format';
import { useDocumentTitle } from '@/lib/use-document-title';
import {
  type APIKeyInfo,
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
  active: { label: '有效', variant: 'success', dot: 'bg-emerald-500' },
  revoked: { label: '已吊销', variant: 'destructive', dot: 'bg-red-500' },
  expired: { label: '已过期', variant: 'secondary', dot: 'bg-zinc-400' },
};

export function APIKeysPage() {
  const { org } = useCurrentOrg();
  const allowed = canManageAPIKeys(org.role);

  useDocumentTitle(`API Keys · ${org.name}`);

  const [issueOpen, setIssueOpen] = useState(false);
  const [issuedSecret, setIssuedSecret] = useState<{ orgId: number; key: IssuedAPIKey } | null>(
    null,
  );
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
        <PageHeader title="API Keys" description="管理用于后端服务或自动化脚本的组织 API 密钥。" />
        <EmptyState
          icon={Lock}
          title="仅 Owner 与 Admin 可管理 API Key"
          description={`你在「${org.name}」的当前角色是 ${ROLE_LABELS[org.role]}，无权查看、签发或吊销组织密钥。`}
        />
      </div>
    );
  }

  const curlSnippet = visibleSecret
    ? `curl -sS -X POST ${window.location.origin}${ENDPOINTS.links.listCreate} \\\n  -H 'Authorization: Bearer ${visibleSecret.api_key}' \\\n  -H 'Content-Type: application/json' \\\n  -d '{"url":"https://example.com"}'`
    : '';

  return (
    <div className="space-y-6">
      <PageHeader
        title="API Keys"
        description="明文密钥只在签发成功时展示一次，系统仅保存不可逆摘要；吊销后立即失效。"
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
            <Button size="sm" onClick={() => setIssueOpen(true)}>
              <Plus className="size-4" />
              签发 API Key
            </Button>
          </>
        }
      />

      {/* 一次性明文密钥 + Resend 风格可直接粘贴的 curl 调试命令 */}
      {visibleSecret && (
        <Card className="border-amber-300/90 bg-amber-50/50">
          <CardHeader className="flex-row items-start justify-between gap-4">
            <div className="flex items-start gap-2.5">
              <span className="mt-0.5 flex size-7 shrink-0 items-center justify-center rounded-lg bg-amber-100 text-amber-800">
                <ShieldAlert className="size-4" />
              </span>
              <div>
                <CardTitle className="text-amber-950">
                  {visibleSecret.name
                    ? `API Key「${visibleSecret.name}」（#${visibleSecret.id}）已签发（明文仅展示这一次）`
                    : `API Key #${visibleSecret.id} 已签发（明文仅展示这一次）`}
                </CardTitle>
                <CardDescription className="text-amber-900/75">
                  请立即保存到密钥管理服务；关闭后服务端无法再次找回明文。
                </CardDescription>
              </div>
            </div>
            <Button variant="outline" size="sm" onClick={() => setIssuedSecret(null)}>
              我已保存，关闭
            </Button>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
              <code
                data-testid="issued-api-key-secret"
                className="flex-1 break-all rounded-lg border border-amber-300/80 bg-white px-3 py-2 font-mono text-xs text-zinc-950"
              >
                {visibleSecret.api_key}
              </code>
              <CopyButton
                value={visibleSecret.api_key}
                variant="pill"
                size="sm"
                label="复制密钥"
                toastMessage="已复制明文 API Key"
              />
            </div>

            <div className="overflow-hidden rounded-lg border border-zinc-800 bg-zinc-950 text-zinc-100">
              <div className="flex items-center justify-between border-b border-zinc-800 px-3 py-1.5 text-[11px] text-zinc-400">
                <span className="inline-flex items-center gap-1.5 font-mono">
                  <Terminal className="size-3.5" />
                  快速验证（cURL）
                </span>
                <CopyButton
                  value={curlSnippet}
                  variant="pill"
                  label="复制命令"
                  toastMessage="已复制 cURL 命令"
                  className="h-6 border-zinc-700 bg-zinc-900 text-zinc-200 hover:bg-zinc-800 hover:text-white"
                />
              </div>
              <pre
                data-testid="issued-api-key-curl"
                className="overflow-x-auto p-3 font-mono text-xs leading-relaxed text-zinc-200"
              >
                {curlSnippet}
              </pre>
            </div>
          </CardContent>
        </Card>
      )}

      {keysQuery.isError && (
        <ErrorAlert
          message={`加载 API Key 失败：${getErrorMessage(keysQuery.error)}`}
          onRetry={() => void keysQuery.refetch()}
        />
      )}

      {keysQuery.isPending ? (
        <div className="space-y-2.5">
          <Skeleton className="h-[72px] w-full rounded-xl" />
          <Skeleton className="h-[72px] w-full rounded-xl" />
        </div>
      ) : keysQuery.data && keysQuery.data.length === 0 ? (
        <EmptyState
          icon={KeyRound}
          title="还没有组织 API Key"
          description="签发一把按最小权限 scope 裁剪的密钥，供后端服务或自动化脚本通过 Bearer Token 调用。"
          action={
            <Button onClick={() => setIssueOpen(true)}>
              <Plus className="size-4" />
              签发第一把 API Key
            </Button>
          }
        />
      ) : keysQuery.data ? (
        <div className="overflow-hidden rounded-xl border border-zinc-200/90 bg-white divide-y divide-zinc-200/80">
          {keysQuery.data.map((key) => {
            const statusMeta = KEY_STATUS_META[key.status];
            return (
              <div
                key={key.id}
                className="flex flex-col gap-3 px-4 py-3.5 transition-colors hover:bg-zinc-50/70 sm:flex-row sm:items-center sm:justify-between"
              >
                <div className="flex min-w-0 items-start gap-3">
                  <span className="mt-0.5 flex size-9 shrink-0 items-center justify-center rounded-lg border border-zinc-200 bg-zinc-50 text-zinc-600">
                    <KeyRound className="size-4" />
                  </span>
                  <div className="min-w-0 space-y-1.5">
                    <div className="flex flex-wrap items-center gap-2">
                      {key.name ? (
                        <>
                          <span className="text-sm font-semibold text-zinc-950">{key.name}</span>
                          <span className="font-mono text-xs text-zinc-400">#{key.id}</span>
                        </>
                      ) : (
                        <span className="font-mono text-sm font-semibold text-zinc-950">
                          #{key.id}
                        </span>
                      )}
                      <Badge
                        variant={statusMeta.variant}
                        className="gap-1.5 px-2 py-0.5 text-[11px]"
                      >
                        <span className={`size-1.5 rounded-full ${statusMeta.dot}`} />
                        {statusMeta.label}
                      </Badge>
                      <span className="text-xs text-zinc-400">
                        {key.created_by
                          ? `由 ${memberNameById.get(key.created_by) ?? `用户 #${key.created_by}`} 签发`
                          : '由管理员 CLI 签发'}
                      </span>
                    </div>

                    <div className="flex flex-wrap items-center gap-1.5">
                      {key.scopes.map((scope) => (
                        <span
                          key={scope}
                          className="rounded-md border border-zinc-200/90 bg-zinc-50 px-1.5 py-0.5 font-mono text-[11px] text-zinc-700"
                        >
                          {scope}
                        </span>
                      ))}
                      <span className="text-zinc-300">·</span>
                      <span
                        title={formatDateTime(key.created_at)}
                        className="text-xs text-zinc-400"
                      >
                        创建于 {formatRelativeTime(key.created_at)}
                      </span>
                      <span className="text-zinc-300">·</span>
                      {key.last_used_at ? (
                        <span
                          title={formatDateTime(key.last_used_at)}
                          className="text-xs text-zinc-500"
                        >
                          最近使用 {formatRelativeTime(key.last_used_at)}
                        </span>
                      ) : (
                        <span className="text-xs text-zinc-400">尚未使用</span>
                      )}
                      {key.expires_at && (
                        <>
                          <span className="text-zinc-300">·</span>
                          <span className="text-xs text-zinc-400">
                            过期 {formatDateTime(key.expires_at)}
                          </span>
                        </>
                      )}
                      {key.revoked_at && (
                        <>
                          <span className="text-zinc-300">·</span>
                          <span className="text-xs text-red-600">
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
                    className="h-8 self-start text-zinc-500 hover:bg-red-50 hover:text-destructive sm:self-center"
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
      ) : null}

      <IssueAPIKeyDialog
        orgId={org.id}
        open={issueOpen}
        onOpenChange={setIssueOpen}
        onIssued={(issued) => setIssuedSecret({ orgId: org.id, key: issued })}
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
