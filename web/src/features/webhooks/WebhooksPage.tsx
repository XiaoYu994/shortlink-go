import { useState } from 'react';
import {
  History,
  KeyRound,
  Lock,
  Pencil,
  Plus,
  RefreshCw,
  Send,
  ShieldAlert,
  Trash2,
  Webhook,
} from 'lucide-react';
import { toast } from 'sonner';
import { useCurrentOrg } from '@/app/org-context';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { CopyButton } from '@/components/CopyButton';
import { EmptyState } from '@/components/EmptyState';
import { ErrorAlert } from '@/components/ErrorAlert';
import { PageHeader } from '@/components/PageHeader';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { useCapabilities } from '@/features/auth/api';
import { canManageWebhooks, ROLE_LABELS } from '@/features/auth/permissions';
import { getErrorMessage } from '@/lib/api-client';
import { cn } from '@/lib/cn';
import { MAX_WEBHOOKS_PER_TENANT } from '@/lib/constants';
import { formatDateTime, formatRelativeTime } from '@/lib/format';
import { useDocumentTitle } from '@/lib/use-document-title';
import {
  useDeleteWebhook,
  useRotateWebhookSecret,
  useTestWebhook,
  useUpdateWebhook,
  useWebhooks,
  type WebhookSubscription,
  type WebhookWithSecret,
} from './api';
import { WebhookDeliveriesDialog } from './WebhookDeliveriesDialog';
import { WebhookFormDialog } from './WebhookFormDialog';

export function WebhooksPage() {
  const { org } = useCurrentOrg();
  const allowed = canManageWebhooks(org.role);
  const capabilitiesQuery = useCapabilities();
  const webhooksEnabled = capabilitiesQuery.isSuccess && capabilitiesQuery.data.features.webhooks;
  const webhooksDisabledByServer =
    capabilitiesQuery.isSuccess && !capabilitiesQuery.data.features.webhooks;

  useDocumentTitle(`Webhooks · ${org.name}`);

  const [formState, setFormState] = useState<{
    orgId: number;
    editing: WebhookSubscription | null;
  } | null>(null);
  const [revealedSecret, setRevealedSecret] = useState<{
    orgId: number;
    webhook: WebhookWithSecret;
  } | null>(null);
  const [rotatingHook, setRotatingHook] = useState<{
    orgId: number;
    webhook: WebhookSubscription;
  } | null>(null);
  const [deletingHook, setDeletingHook] = useState<{
    orgId: number;
    webhook: WebhookSubscription;
  } | null>(null);
  const [deliveriesHook, setDeliveriesHook] = useState<{
    orgId: number;
    webhook: WebhookSubscription;
  } | null>(null);

  const visibleSecret =
    revealedSecret && revealedSecret.orgId === org.id ? revealedSecret.webhook : null;
  const activeForm = formState && formState.orgId === org.id ? formState : null;
  const activeRotating =
    rotatingHook && rotatingHook.orgId === org.id ? rotatingHook.webhook : null;
  const activeDeleting =
    deletingHook && deletingHook.orgId === org.id ? deletingHook.webhook : null;
  const activeDeliveries =
    deliveriesHook && deliveriesHook.orgId === org.id ? deliveriesHook.webhook : null;

  const webhooksQuery = useWebhooks(org.id, allowed && webhooksEnabled);
  const updateWebhook = useUpdateWebhook(org.id);
  const deleteWebhook = useDeleteWebhook(org.id);
  const rotateSecret = useRotateWebhookSecret(org.id);
  const testWebhook = useTestWebhook(org.id);

  async function handleToggleEnabled(sub: WebhookSubscription) {
    try {
      await updateWebhook.mutateAsync({ id: sub.id, enabled: !sub.enabled });
      toast.success(sub.enabled ? `已停用 Webhook #${sub.id}` : `已启用 Webhook #${sub.id}`);
    } catch (err) {
      toast.error(getErrorMessage(err));
    }
  }

  async function handleSendTest(sub: WebhookSubscription) {
    try {
      const res = await testWebhook.mutateAsync(sub.id);
      toast.success(`已入队测试 Ping（${res.delivery_id}），可在投递记录中查看结果`);
    } catch (err) {
      toast.error(getErrorMessage(err));
    }
  }

  async function handleConfirmRotate() {
    if (!activeRotating) return;
    try {
      const rotated = await rotateSecret.mutateAsync(activeRotating.id);
      setRevealedSecret({ orgId: org.id, webhook: rotated });
      setRotatingHook(null);
      toast.success(`已轮换 Webhook #${rotated.id} 的签名密钥`);
    } catch (err) {
      toast.error(getErrorMessage(err));
    }
  }

  async function handleConfirmDelete() {
    if (!activeDeleting) return;
    try {
      await deleteWebhook.mutateAsync(activeDeleting.id);
      toast.success(`已删除 Webhook #${activeDeleting.id}`);
      setDeletingHook(null);
    } catch (err) {
      toast.error(getErrorMessage(err));
    }
  }

  if (!allowed) {
    return (
      <div className="space-y-6">
        <PageHeader
          title="Webhooks"
          description="将短链创建、变更与安全封禁事件实时推送到外部系统。"
        />
        <EmptyState
          icon={Lock}
          title="仅 Owner 与 Admin 可管理 Webhooks"
          description={`你在「${org.name}」的当前角色是 ${ROLE_LABELS[org.role]}，无权查看或配置组织 Webhook 订阅。`}
        />
      </div>
    );
  }

  if (capabilitiesQuery.isError) {
    return (
      <div className="space-y-6">
        <PageHeader
          title="Webhooks"
          description="将短链创建、变更与安全封禁事件实时推送到外部系统。"
        />
        <ErrorAlert
          message={`无法获取服务端功能配置：${getErrorMessage(capabilitiesQuery.error)}`}
          onRetry={() => void capabilitiesQuery.refetch()}
        />
      </div>
    );
  }

  if (webhooksDisabledByServer) {
    return (
      <div className="space-y-6">
        <PageHeader
          title="Webhooks"
          description="将短链创建、变更与安全封禁事件实时推送到外部系统。"
        />
        <EmptyState
          icon={Webhook}
          title="服务端尚未启用 Webhook 功能"
          description="请管理员在服务端环境变量配置 32 字节（64 位十六进制）WEBHOOK_SECRET_KEY 后重启控制台与 worker。"
        />
      </div>
    );
  }

  const reachedLimit = (webhooksQuery.data?.length ?? 0) >= MAX_WEBHOOKS_PER_TENANT;

  return (
    <div className="space-y-6">
      <PageHeader
        title="Webhooks"
        description={`每个组织最多配置 ${MAX_WEBHOOKS_PER_TENANT} 个订阅；出站请求携带 HMAC-SHA256 签名，连续 10 次终态失败将自动熔断停用。`}
        actions={
          <>
            <Button
              variant="outline"
              size="sm"
              disabled={!webhooksEnabled || webhooksQuery.isFetching}
              onClick={() => void webhooksQuery.refetch()}
            >
              <RefreshCw className={cn('size-3.5', webhooksQuery.isFetching && 'animate-spin')} />
              刷新
            </Button>
            <Button
              size="sm"
              disabled={!webhooksEnabled || reachedLimit}
              onClick={() => {
                setFormState({ orgId: org.id, editing: null });
              }}
            >
              <Plus className="size-4" />
              添加订阅
            </Button>
          </>
        }
      />

      {visibleSecret && (
        <Card className="border-amber-200 bg-amber-50/70">
          <CardHeader>
            <div className="flex items-start justify-between gap-4">
              <div className="flex items-center gap-2 text-amber-900">
                <ShieldAlert className="size-5 shrink-0" />
                <CardTitle>请立即保存 Webhook #{visibleSecret.id} 的签名密钥</CardTitle>
              </div>
              <Button variant="ghost" size="sm" onClick={() => setRevealedSecret(null)}>
                我已保存
              </Button>
            </div>
            <CardDescription className="text-amber-800">
              明文密钥只在创建或轮换时显示这一次。接收端需使用它校验请求头{' '}
              <code className="font-mono">
                X-Shortlink-Signature: t=&lt;unix&gt;,v1=&lt;hex&gt;
              </code>
              （对 <code className="font-mono">{'<t>.<body>'}</code> 做 HMAC-SHA256）。
            </CardDescription>
          </CardHeader>
          <CardContent>
            <div className="flex items-center gap-2 rounded-lg border border-amber-200 bg-white px-3 py-2">
              <code
                data-testid="webhook-plaintext-secret"
                className="min-w-0 flex-1 truncate font-mono text-xs text-zinc-900"
              >
                {visibleSecret.secret}
              </code>
              <CopyButton
                value={visibleSecret.secret}
                variant="pill"
                size="sm"
                label="复制密钥"
                toastMessage="已复制 Webhook 签名密钥"
              />
            </div>
          </CardContent>
        </Card>
      )}

      {webhooksQuery.isError ? (
        <ErrorAlert
          message={`加载 Webhook 列表失败：${getErrorMessage(webhooksQuery.error)}`}
          onRetry={() => void webhooksQuery.refetch()}
        />
      ) : webhooksQuery.isPending ? (
        <Skeleton className="h-24 w-full rounded-xl" />
      ) : webhooksQuery.data.length === 0 ? (
        <EmptyState
          icon={Webhook}
          title="还没有 Webhook 订阅"
          description="配置一条 HTTPS 回调地址，即可在短链创建、更新、删除或被风控自动封禁时收到可靠通知。"
          action={
            <Button
              size="sm"
              onClick={() => {
                setFormState({ orgId: org.id, editing: null });
              }}
            >
              <Plus className="size-4" />
              添加第一条订阅
            </Button>
          }
        />
      ) : (
        <div className="divide-y divide-zinc-200/80 overflow-hidden rounded-xl border border-zinc-200/90 bg-white">
          {webhooksQuery.data.map((sub) => (
            <div
              key={sub.id}
              className="flex flex-col gap-3 px-4 py-4 transition-colors hover:bg-zinc-50/70 lg:flex-row lg:items-center lg:justify-between"
            >
              <div className="min-w-0 space-y-1.5">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="truncate font-mono text-sm font-medium text-zinc-950">
                    {sub.url}
                  </span>
                  {sub.enabled ? (
                    <Badge variant="success" className="text-[11px]">
                      已启用
                    </Badge>
                  ) : sub.disabled_reason === 'consecutive_failures' ? (
                    <Badge variant="destructive" className="text-[11px]">
                      连续失败自动熔断
                    </Badge>
                  ) : (
                    <Badge variant="secondary" className="text-[11px]">
                      已手动停用
                    </Badge>
                  )}
                  <span className="font-mono text-xs text-zinc-400">#{sub.id}</span>
                  {sub.consecutive_failures > 0 && (
                    <span className="text-xs text-amber-700">
                      连续失败 {sub.consecutive_failures} 次
                    </span>
                  )}
                </div>

                {sub.description && <p className="text-xs text-zinc-600">{sub.description}</p>}

                <div className="flex flex-wrap items-center gap-1.5">
                  {sub.events.map((ev) => (
                    <Badge key={ev} variant="outline" className="font-mono text-[11px]">
                      {ev}
                    </Badge>
                  ))}
                  <span className="font-mono text-[11px] text-zinc-400">
                    密钥前缀 {sub.secret_prefix}…
                  </span>
                  <span className="text-zinc-300">·</span>
                  <span className="text-xs text-zinc-500" title={formatDateTime(sub.created_at)}>
                    创建于 {formatRelativeTime(sub.created_at)}
                  </span>
                </div>
              </div>

              <div className="flex flex-wrap items-center gap-1.5 self-start lg:self-center">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={testWebhook.isPending}
                  onClick={() => void handleSendTest(sub)}
                >
                  <Send className="size-3.5" />
                  测试 Ping
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setDeliveriesHook({ orgId: org.id, webhook: sub })}
                >
                  <History className="size-3.5" />
                  投递记录
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={updateWebhook.isPending}
                  onClick={() => void handleToggleEnabled(sub)}
                >
                  {sub.enabled ? '停用' : '重新启用'}
                </Button>
                <Button
                  variant="ghost"
                  size="icon"
                  title="编辑订阅"
                  aria-label="编辑订阅"
                  onClick={() => {
                    setFormState({ orgId: org.id, editing: sub });
                  }}
                >
                  <Pencil className="size-3.5" />
                </Button>
                <Button
                  variant="ghost"
                  size="icon"
                  title="轮换签名密钥"
                  aria-label="轮换签名密钥"
                  onClick={() => setRotatingHook({ orgId: org.id, webhook: sub })}
                >
                  <KeyRound className="size-3.5" />
                </Button>
                <Button
                  variant="ghost"
                  size="icon"
                  title="删除订阅"
                  aria-label="删除订阅"
                  className="text-destructive hover:bg-red-50 hover:text-destructive"
                  onClick={() => setDeletingHook({ orgId: org.id, webhook: sub })}
                >
                  <Trash2 className="size-3.5" />
                </Button>
              </div>
            </div>
          ))}
        </div>
      )}

      {activeForm && (
        <WebhookFormDialog
          key={activeForm.editing ? `edit-${activeForm.editing.id}` : 'create'}
          orgId={org.id}
          open
          editing={activeForm.editing}
          onOpenChange={(nextOpen) => {
            if (!nextOpen) setFormState(null);
          }}
          onCreated={(created) => {
            setRevealedSecret({ orgId: org.id, webhook: created });
          }}
        />
      )}

      <WebhookDeliveriesDialog
        orgId={org.id}
        webhook={activeDeliveries}
        onOpenChange={(nextOpen) => {
          if (!nextOpen) setDeliveriesHook(null);
        }}
      />

      <ConfirmDialog
        open={activeRotating !== null}
        onOpenChange={(nextOpen) => {
          if (!nextOpen) setRotatingHook(null);
        }}
        title={`轮换 Webhook #${activeRotating?.id ?? ''} 的签名密钥？`}
        description="轮换后会立即生成新签名密钥并覆盖旧密钥，请同步更新接收端配置。"
        confirmLabel="确认轮换"
        pending={rotateSecret.isPending}
        onConfirm={handleConfirmRotate}
      />

      <ConfirmDialog
        open={activeDeleting !== null}
        onOpenChange={(nextOpen) => {
          if (!nextOpen) setDeletingHook(null);
        }}
        title={`删除 Webhook #${activeDeleting?.id ?? ''}？`}
        description="删除后不再向该地址投递事件，排队中的未投递任务也会自动跳过。"
        confirmLabel="确认删除"
        pending={deleteWebhook.isPending}
        onConfirm={handleConfirmDelete}
      />
    </div>
  );
}
