import { Badge, type BadgeProps } from '@/components/ui/badge';
import type { LinkStatus } from './api';
import { formatBanReason } from './link-labels';

const STATUS_META: Record<
  LinkStatus,
  { label: string; variant: BadgeProps['variant']; dotClass: string }
> = {
  active: { label: '正常可跳转', variant: 'success', dotClass: 'bg-emerald-600' },
  disabled: { label: '已停用', variant: 'warning', dotClass: 'bg-amber-600' },
  expired: { label: '已过期', variant: 'secondary', dotClass: 'bg-ink-muted' },
  banned: { label: '已封禁', variant: 'stamp', dotClass: 'bg-stamp-red' },
};

interface LinkStatusBadgeProps {
  status: LinkStatus;
  banReason?: string;
  // compact 模式下正常链接仅展示克制的翠绿脉动小点，把视觉注意力留给异常状态与红色封印印章。
  compact?: boolean;
  // hideBanReason 在紧凑行视图下将封禁原因收进 title，避免固定窄列中徽章折行。
  hideBanReason?: boolean;
}

export function LinkStatusBadge({
  status,
  banReason,
  compact = false,
  hideBanReason = false,
}: LinkStatusBadgeProps) {
  const meta = STATUS_META[status];

  if (compact && status === 'active') {
    return (
      <span
        title="正常可跳转"
        aria-label="状态：正常"
        className="inline-flex size-2.5 shrink-0 rounded-full border border-ink/50 bg-emerald-500 ring-2 ring-hl-mint"
      />
    );
  }

  const localizedReason = formatBanReason(banReason);
  const inlineReason = !hideBanReason && localizedReason ? ` · ${localizedReason}` : '';

  return (
    <span
      className="inline-flex shrink-0 items-center"
      title={hideBanReason && localizedReason ? `封禁原因：${localizedReason}` : undefined}
    >
      <Badge variant={meta.variant} className="gap-1.5 whitespace-nowrap px-2 py-0.5 text-[11px]">
        {status !== 'banned' && <span className={`size-1.5 rounded-full ${meta.dotClass}`} />}
        {meta.label}
        {inlineReason}
      </Badge>
    </span>
  );
}
