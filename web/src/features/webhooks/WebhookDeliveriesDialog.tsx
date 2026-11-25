import { History, RefreshCw } from 'lucide-react';
import { EmptyState } from '@/components/EmptyState';
import { ErrorAlert } from '@/components/ErrorAlert';
import { Badge, type BadgeProps } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import { Skeleton } from '@/components/ui/skeleton';
import { getErrorMessage } from '@/lib/api-client';
import { cn } from '@/lib/cn';
import { formatDateTime, formatRelativeTime } from '@/lib/format';
import {
  useInfiniteWebhookDeliveries,
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
  const items = deliveriesQuery.data?.pages.flatMap((page) => page.items) ?? [];

  return (
    <Dialog open={webhook !== null} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <div className="flex items-center justify-between gap-3 pr-6">
            <div>
              <DialogTitle>投递记录 · Webhook #{webhook?.id}</DialogTitle>
              <DialogDescription className="mt-1 truncate font-mono text-xs">
                {webhook?.url}
              </DialogDescription>
            </div>
            <Button
              variant="outline"
              size="sm"
              disabled={deliveriesQuery.isFetching}
              onClick={() => void deliveriesQuery.refetch()}
            >
              <RefreshCw className={cn('size-3.5', deliveriesQuery.isFetching && 'animate-spin')} />
              刷新
            </Button>
          </div>
        </DialogHeader>

        <div className="max-h-[60vh] space-y-3 overflow-y-auto pr-1">
          {deliveriesQuery.isError ? (
            <ErrorAlert
              message={`加载投递记录失败：${getErrorMessage(deliveriesQuery.error)}`}
              onRetry={() => void deliveriesQuery.refetch()}
            />
          ) : deliveriesQuery.isPending ? (
            <Skeleton className="h-24 w-full rounded-xl" />
          ) : items.length === 0 ? (
            <EmptyState
              icon={History}
              title="暂无投递记录"
              description="当短链发生订阅事件或点击「测试 Ping」后，投递明细会记录在这里。"
            />
          ) : (
            <div className="divide-y divide-zinc-200/80 rounded-xl border border-zinc-200/90 bg-white">
              {items.map((d) => {
                const statusMeta = DELIVERY_STATUS_META[d.status];
                return (
                  <div key={d.id} className="space-y-1.5 px-4 py-3 text-xs">
                    <div className="flex flex-wrap items-center justify-between gap-2">
                      <div className="flex flex-wrap items-center gap-2">
                        <Badge variant={statusMeta.variant} className="text-[11px]">
                          {statusMeta.label}
                        </Badge>
                        <span className="rounded bg-zinc-100 px-1.5 py-0.5 font-mono text-zinc-800">
                          {d.event}
                        </span>
                        {d.http_status !== undefined && (
                          <span className="font-mono text-zinc-700">HTTP {d.http_status}</span>
                        )}
                        <span className="text-zinc-500">
                          第 {d.attempt} 次 · {d.duration_ms} ms
                        </span>
                      </div>
                      <span className="text-zinc-500" title={formatDateTime(d.created_at)}>
                        {formatRelativeTime(d.created_at)}
                      </span>
                    </div>

                    <div className="font-mono text-[11px] text-zinc-500">
                      {d.delivery_id} · job #{d.job_id}
                    </div>

                    {d.error_message && (
                      <div className="rounded-md bg-red-50/70 px-2.5 py-1.5 font-mono text-[11px] text-red-700">
                        {d.error_message}
                      </div>
                    )}
                    {d.response_snippet && (
                      <pre className="max-h-24 overflow-auto rounded-md bg-zinc-50 px-2.5 py-1.5 font-mono text-[11px] text-zinc-700">
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
      </DialogContent>
    </Dialog>
  );
}
