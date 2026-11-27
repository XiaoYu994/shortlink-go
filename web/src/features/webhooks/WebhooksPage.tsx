import { useState } from 'react';
import {
  History,
  KeyRound,
  Lock,
  MoreHorizontal,
  Pencil,
  Plus,
  RefreshCw,
  Send,
  Trash2,
  Webhook,
} from 'lucide-react';
import { toast } from 'sonner';
import { useCurrentOrg } from '@/app/org-context';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { CourierCat } from '@/components/CourierCat';
import { EmptyState } from '@/components/EmptyState';
import { ErrorAlert } from '@/components/ErrorAlert';
import { OneTimeSecretBanner } from '@/components/OneTimeSecretBanner';
import { PageHeader } from '@/components/PageHeader';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { Skeleton } from '@/components/ui/skeleton';
import { Switch } from '@/components/ui/switch';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
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
      toast.success(`已发送测试请求（${res.delivery_id}），可在投递记录中查看结果`);
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
      toast.success(`已更换 Webhook #${rotated.id} 的签名密钥`);
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
        <PageHeader title="Webhooks" />
        <EmptyState
          icon={Lock}
          illustration={<CourierCat pose="shield" />}
          title="仅所有者与管理员可管理 Webhooks"
          description={`你在「${org.name}」的当前角色是 ${ROLE_LABELS[org.role]}，无权查看或配置 Webhook 订阅。`}
        />
      </div>
    );
  }

  if (capabilitiesQuery.isError) {
    return (
      <div className="space-y-6">
        <PageHeader title="Webhooks" />
        <ErrorAlert
          message={`功能状态加载失败：${getErrorMessage(capabilitiesQuery.error)}`}
          onRetry={() => void capabilitiesQuery.refetch()}
        />
      </div>
    );
  }

  if (webhooksDisabledByServer) {
    return (
      <div className="space-y-6">
        <PageHeader title="Webhooks" />
        <EmptyState
          icon={Webhook}
          illustration={<CourierCat pose="shield" />}
          title="Webhook 功能暂未开启"
          description="当前工作空间尚未开启 Webhook 事件推送功能，如需开通请联系管理员。"
        />
      </div>
    );
  }

  const count = webhooksQuery.data?.length ?? 0;
  const reachedLimit = count >= MAX_WEBHOOKS_PER_TENANT;

  const addBtn = reachedLimit ? (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="inline-flex">
          <Button size="sm" variant="highlight" disabled>
            <Plus className="size-4" />
            添加订阅
          </Button>
        </span>
      </TooltipTrigger>
      <TooltipContent>
        已达组织订阅上限（{count} / {MAX_WEBHOOKS_PER_TENANT}），如需新增请先删除闲置订阅
      </TooltipContent>
    </Tooltip>
  ) : (
    <Button
      size="sm"
      variant="highlight"
      disabled={!webhooksEnabled}
      onClick={() => setFormState({ orgId: org.id, editing: null })}
    >
      <Plus className="size-4" />
      添加订阅
    </Button>
  );

  return (
    <div className="space-y-6">
      <PageHeader
        title="Webhooks"
        actions={
          <>
            {webhooksQuery.data && (
              <Badge variant="outline" className="font-mono text-xs">
                {count} / {MAX_WEBHOOKS_PER_TENANT}
              </Badge>
            )}
            <Button
              variant="outline"
              size="sm"
              disabled={!webhooksEnabled || webhooksQuery.isFetching}
              onClick={() => void webhooksQuery.refetch()}
            >
              <RefreshCw className={cn('size-3.5', webhooksQuery.isFetching && 'animate-spin')} />
              刷新
            </Button>
            {addBtn}
          </>
        }
      />

      {visibleSecret && (
        <OneTimeSecretBanner
          title={`请立即保存 Webhook #${visibleSecret.id} 的签名密钥`}
          description={
            <>
              明文密钥仅在创建或更换时展示一次。接收端可通过请求头{' '}
              <code className="font-mono text-ink">
                X-Shortlink-Signature: t=&lt;unix&gt;,v1=&lt;hex&gt;
              </code>{' '}
              校验推送来源与内容完整性。
            </>
          }
          secret={visibleSecret.secret}
          secretTestId="webhook-plaintext-secret"
          copyLabel="复制密钥"
          copyToastMessage="已复制 Webhook 签名密钥"
          onDismiss={() => setRevealedSecret(null)}
        />
      )}

      {webhooksQuery.isError ? (
        <ErrorAlert
          message={`加载 Webhook 列表失败：${getErrorMessage(webhooksQuery.error)}`}
          onRetry={() => void webhooksQuery.refetch()}
        />
      ) : webhooksQuery.isPending ? (
        <Skeleton className="h-24 w-full rounded-2xl" />
      ) : webhooksQuery.data.length === 0 ? (
        <EmptyState
          icon={Webhook}
          illustration={<CourierCat pose="empty" />}
          title="还没有 Webhook 订阅"
          description="配置一条 HTTPS 回调地址，即可在短链创建、更新、删除或被风控自动封禁时收到可靠通知。"
          action={
            <Button
              size="sm"
              variant="highlight"
              onClick={() => setFormState({ orgId: org.id, editing: null })}
            >
              <Plus className="size-4" />
              添加第一条订阅
            </Button>
          }
        />
      ) : (
        <div className="divide-y divide-border-soft overflow-hidden rounded-2xl border-[1.25px] border-ink bg-card">
          {webhooksQuery.data.map((sub) => {
            const circuitBroken = !sub.enabled && sub.disabled_reason === 'consecutive_failures';
            return (
              <div
                key={sub.id}
                className={cn(
                  'space-y-3 px-4 py-4 transition-colors hover:bg-paper-warm/45',
                  circuitBroken && 'bg-red-50/35',
                )}
              >
                <div className="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
                  <div className="min-w-0 space-y-1.5">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="truncate font-mono text-sm font-bold text-ink">
                        {sub.url}
                      </span>
                      {sub.enabled ? (
                        <Badge variant="success" className="text-[11px]">
                          已启用
                        </Badge>
                      ) : circuitBroken ? (
                        <Badge variant="destructive" className="text-[11px]">
                          连续失败已暂停
                        </Badge>
                      ) : (
                        <Badge variant="secondary" className="text-[11px]">
                          已手动停用
                        </Badge>
                      )}
                      <span className="font-mono text-xs text-muted-foreground">#{sub.id}</span>
                      {sub.consecutive_failures > 0 && (
                        <span className="rounded bg-hl-peach/55 px-1.5 py-0.5 text-xs font-medium text-ink">
                          连续失败 {sub.consecutive_failures} 次
                        </span>
                      )}
                    </div>

                    {sub.description && (
                      <p className="text-xs text-ink-secondary">{sub.description}</p>
                    )}

                    <div className="flex flex-wrap items-center gap-1.5">
                      {sub.events.map((ev) => (
                        <Badge key={ev} variant="outline" className="font-mono text-[11px]">
                          {ev}
                        </Badge>
                      ))}
                      <span className="font-mono text-[11px] text-muted-foreground">
                        密钥前缀 {sub.secret_prefix}…
                      </span>
                      <span className="text-ink/30">·</span>
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <span className="text-xs text-muted-foreground">
                            创建于 {formatRelativeTime(sub.created_at)}
                          </span>
                        </TooltipTrigger>
                        <TooltipContent>{formatDateTime(sub.created_at)}</TooltipContent>
                      </Tooltip>
                    </div>
                  </div>

                  <div className="flex shrink-0 items-center gap-2 self-start lg:self-center">
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => setDeliveriesHook({ orgId: org.id, webhook: sub })}
                    >
                      <History className="size-3.5" />
                      投递记录
                    </Button>

                    <Tooltip>
                      <TooltipTrigger asChild>
                        <span className="inline-flex items-center">
                          <Switch
                            aria-label={`${sub.enabled ? '停用' : '启用'} Webhook #${sub.id}`}
                            checked={sub.enabled}
                            disabled={updateWebhook.isPending}
                            onCheckedChange={() => void handleToggleEnabled(sub)}
                          />
                        </span>
                      </TooltipTrigger>
                      <TooltipContent>{sub.enabled ? '点击停用' : '点击重新启用'}</TooltipContent>
                    </Tooltip>

                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <button
                          type="button"
                          aria-label={`操作 Webhook #${sub.id}`}
                          className="inline-flex size-8 items-center justify-center rounded-lg border-[1.25px] border-transparent text-ink-secondary transition-colors hover:border-ink/60 hover:bg-paper-warm hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink cursor-pointer"
                        >
                          <MoreHorizontal className="size-4" />
                        </button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end" className="w-44">
                        <DropdownMenuItem
                          disabled={testWebhook.isPending}
                          onSelect={() => void handleSendTest(sub)}
                        >
                          <Send className="size-4 text-ink-secondary" />
                          发送测试请求
                        </DropdownMenuItem>
                        <DropdownMenuItem
                          onSelect={() => setFormState({ orgId: org.id, editing: sub })}
                        >
                          <Pencil className="size-4 text-ink-secondary" />
                          编辑订阅
                        </DropdownMenuItem>
                        <DropdownMenuItem
                          onSelect={() => setRotatingHook({ orgId: org.id, webhook: sub })}
                        >
                          <KeyRound className="size-4 text-ink-secondary" />
                          更换签名密钥
                        </DropdownMenuItem>
                        <DropdownMenuSeparator />
                        <DropdownMenuItem
                          onSelect={() => setDeletingHook({ orgId: org.id, webhook: sub })}
                          className="text-stamp-red focus:bg-red-50 focus:text-stamp-red"
                        >
                          <Trash2 className="size-4" />
                          删除订阅
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </div>
                </div>

                {circuitBroken && (
                  <div className="rounded-xl border border-stamp-red/45 bg-red-50/70 px-3 py-2 text-xs text-[#991b1b]">
                    恢复指引：1. 点「投递记录」排查最近失败原因 → 2. 确认接收服务恢复正常 → 3.
                    打开右侧开关并在抽屉内点「发送测试请求」验证连通性。
                  </div>
                )}
              </div>
            );
          })}
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
        title={`更换 Webhook #${activeRotating?.id ?? ''} 的签名密钥？`}
        description="更换后会立即生成新签名密钥并使旧密钥失效，请同步更新接收端配置。"
        confirmLabel="确认更换"
        pending={rotateSecret.isPending}
        onConfirm={handleConfirmRotate}
      />

      <ConfirmDialog
        open={activeDeleting !== null}
        onOpenChange={(nextOpen) => {
          if (!nextOpen) setDeletingHook(null);
        }}
        title={`删除 Webhook #${activeDeleting?.id ?? ''}？`}
        description="删除后将停止向该地址推送事件，尚未完成的投递也会自动取消。"
        confirmLabel="确认删除"
        pending={deleteWebhook.isPending}
        onConfirm={handleConfirmDelete}
      />
    </div>
  );
}
