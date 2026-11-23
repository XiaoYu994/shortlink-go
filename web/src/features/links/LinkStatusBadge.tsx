import { Badge, type BadgeProps } from '@/components/ui/badge';
import type { LinkStatus } from './api';

const STATUS_META: Record<
  LinkStatus,
  { label: string; variant: BadgeProps['variant']; dotClass: string }
> = {
  active: { label: '正常', variant: 'success', dotClass: 'bg-emerald-500' },
  disabled: { label: '已停用', variant: 'warning', dotClass: 'bg-amber-500' },
  expired: { label: '已过期', variant: 'secondary', dotClass: 'bg-zinc-400' },
  banned: { label: '已封禁', variant: 'destructive', dotClass: 'bg-red-500' },
};

interface LinkStatusBadgeProps {
  status: LinkStatus;
  banReason?: string;
  // compact 模式下正常链接只展示克制的绿色小圆点，把视觉注意力留给异常状态。
  compact?: boolean;
}

export function LinkStatusBadge({ status, banReason, compact = false }: LinkStatusBadgeProps) {
  const meta = STATUS_META[status];

  if (compact && status === 'active') {
    return (
      <span
        title="正常可跳转"
        aria-label="状态：正常"
        className="inline-flex size-2 shrink-0 rounded-full bg-emerald-500 ring-2 ring-emerald-500/20"
      />
    );
  }

  return (
    <span className="inline-flex items-center gap-1.5">
      <Badge variant={meta.variant} className="gap-1.5 px-2 py-0.5 text-[11px]">
        <span className={`size-1.5 rounded-full ${meta.dotClass}`} />
        {meta.label}
        {banReason ? ` · ${banReason}` : ''}
      </Badge>
    </span>
  );
}
