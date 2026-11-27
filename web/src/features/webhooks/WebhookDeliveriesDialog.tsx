import { useEffect, useState } from 'react';
import { History, RefreshCw, Send } from 'lucide-react';
import { toast } from 'sonner';
import { CourierCat } from '@/components/CourierCat';
import { EmptyState } from '@/components/EmptyState';
import { ErrorAlert } from '@/components/ErrorAlert';
import { Badge, type BadgeProps } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet';
import { Skeleton } from '@/components/ui/skeleton';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import { getErrorMessage } from '@/lib/api-client';
import { cn } from '@/lib/cn';
import { formatDateTime, formatRelativeTime } from '@/lib/format';
import {
  useInfiniteWebhookDeliveries,
  useTestWebhook,
  type WebhookDeliveryStatus,
  type WebhookSubscription,
} from './api';

const DELIVERY_STATUS_META: Record<
  WebhookDeliveryStatus,
  { label: string; variant: BadgeProps['variant'] }
> = {
  success: { label: '成功', variant: 'success' },
  retry: { label: '待重试', variant: 'warning' },
  failed: { label: '失败', variant: 'destructive' },
};

interface WebhookDeliveriesDialogProps {
  orgId: number;
  webhook: WebhookSubscription | null;
  onOpenChange: (open: boolean) => void;
}

export function WebhookDeliveriesDialog({
  orgId,
  webhook,
  onOpenChange,
}: WebhookDeliveriesDialogProps) {
  const deliveriesQuery = useInfiniteWebhookDeliveries(
    orgId,
    webhook?.id ?? null,
    webhook !== null,
  );
  const testWebhook = useTestWebhook(orgId);
  const [pollingState, setPollingState] = useState<{
    webhookId: number;
    deliveryId: string;
    remaining: number;
  } | null>(null);

  const { refetch } = deliveriesQuery;
  const items = deliveriesQuery.data?.pages.flatMap((page) => page.items) ?? [];
  const activeWebhookId = webhook?.id ?? null;
  const targetArrived = Boolean(
    pollingState && items.some((d) => d.delivery_id === pollingState.deliveryId),
  );
  const shouldPoll = Boolean(
    pollingState &&
    activeWebhookId !== null &&
    pollingState.webhookId === activeWebhookId &&
    pollingState.remaining > 0 &&
    !targetArrived,
  );

  // 发送测试 Ping 后自动短轮询，直到收到对应 delivery_id 或最多尝试 5 次。
  useEffect(() => {
    if (!shouldPoll) return;
    const timer = window.setInterval(() => {
      void refetch();
      setPollingState((prev) => {
        if (!prev || prev.remaining <= 1) return null;
        return { ...prev, remaining: prev.remaining - 1 };
      });
    }, 1500);
    return () => window.clearInterval(timer);
  }, [shouldPoll, refetch]);

  async function handleDrawerTestPing() {
    if (!webhook) return;
    try {
      const res = await testWebhook.mutateAsync(webhook.id);
      toast.success(`已发送测试请求（${res.delivery_id}），正在自动刷新结果`);
      setPollingState({ webhookId: webhook.id, deliveryId: res.delivery_id, remaining: 5 });
    } catch (err) {
      toast.error(getErrorMessage(err));
    }
  }

  return (
    <Sheet
      open={webhook !== null}
      onOpenChange={(nextOpen) => {
        if (!nextOpen) setPollingState(null);
        onOpenChange(nextOpen);
      }}
    >
      <SheetContent side="right" className="max-w-2xl">
        <SheetHeader className="pr-14">
          <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
            <div className="min-w-0">
              <SheetTitle>投递记录 · Webhook #{webhook?.id}</SheetTitle>
              <SheetDescription className="mt-1 truncate font-mono">
                {webhook?.url}
              </SheetDescription>
            </div>
            <div className="flex shrink-0 items-center gap-2">
              <Button
                variant="highlight"
                size="sm"
                disabled={!webhook || testWebhook.isPending}
                onClick={() => void handleDrawerTestPing()}
              >
                <Send className="size-3.5" />
                发送测试请求
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={deliveriesQuery.isFetching}
                onClick={() => void deliveriesQuery.refetch()}
              >
                <RefreshCw
                  className={cn('size-3.5', deliveriesQuery.isFetching && 'animate-spin')}
                />
                刷新
              </Button>
            </div>
          </div>
        </SheetHeader>

        <div className="flex-1 space-y-3 overflow-y-auto p-6">
          {shouldPoll && (
            <div className="flex items-center gap-2 rounded-xl border border-ink/50 bg-hl-yellow/45 px-3 py-2 text-xs font-medium text-ink">
              <RefreshCw className="size-3.5 animate-spin" />
              正在等待投递结果（{pollingState?.deliveryId}）…
            </div>
          )}

          {deliveriesQuery.isError ? (
            <ErrorAlert
              message={`加载投递记录失败：${getErrorMessage(deliveriesQuery.error)}`}
              onRetry={() => void deliveriesQuery.refetch()}
            />
          ) : deliveriesQuery.isPending ? (
            <div className="space-y-2.5">
              <Skeleton className="h-20 w-full rounded-xl" />
              <Skeleton className="h-20 w-full rounded-xl" />
            </div>
          ) : items.length === 0 ? (
            <EmptyState
              icon={History}
              illustration={<CourierCat pose="empty" />}
              title="暂无投递记录"
              description="点击右上角「发送测试请求」或等待短链发生订阅事件后，投递明细会记录在这里。"
            />
          ) : (
            <div className="divide-y divide-border-soft overflow-hidden rounded-2xl border-[1.25px] border-ink bg-card">
              {items.map((d) => {
                const statusMeta = DELIVERY_STATUS_META[d.status];
                return (
                  <div key={d.id} className="space-y-1.5 px-4 py-3 text-xs">
                    <div className="flex flex-wrap items-center justify-between gap-2">
                      <div className="flex flex-wrap items-center gap-2">
                        <Badge variant={statusMeta.variant} className="text-[11px]">
                          {statusMeta.label}
                        </Badge>
                        <span className="rounded border border-ink/25 bg-paper-warm px-1.5 py-0.5 font-mono font-medium text-ink">
                          {d.event}
                        </span>
                        {d.http_status !== undefined && (
                          <span className="font-mono font-semibold text-ink">
                            HTTP {d.http_status}
                          </span>
                        )}
                        <span className="text-ink-secondary">
                          第 {d.attempt} 次 · {d.duration_ms} ms
                        </span>
                      </div>
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <span className="text-muted-foreground">
                            {formatRelativeTime(d.created_at)}
                          </span>
                        </TooltipTrigger>
                        <TooltipContent>{formatDateTime(d.created_at)}</TooltipContent>
                      </Tooltip>
                    </div>

                    <div className="font-mono text-[11px] text-muted-foreground">
                      投递编号：{d.delivery_id}
                    </div>

                    {d.error_message && (
                      <div className="rounded-lg border border-stamp-red/40 bg-red-50/75 px-2.5 py-1.5 font-mono text-[11px] text-[#991b1b]">
                        {d.error_message}
                      </div>
                    )}
                    {d.response_snippet && (
                      <pre className="max-h-28 overflow-auto whitespace-pre-wrap break-all rounded-lg border border-border-soft bg-paper-warm/70 px-2.5 py-1.5 font-mono text-[11px] text-ink">
                        {d.response_snippet}
                      </pre>
                    )}
                  </div>
                );
              })}
            </div>
          )}

          {deliveriesQuery.hasNextPage && (
            <div className="flex justify-center pt-2">
              <Button
                variant="outline"
                size="sm"
                disabled={deliveriesQuery.isFetchingNextPage}
                onClick={() => void deliveriesQuery.fetchNextPage()}
              >
                {deliveriesQuery.isFetchingNextPage ? '加载中…' : '加载更多记录'}
              </Button>
            </div>
          )}
        </div>
      </SheetContent>
    </Sheet>
  );
}
