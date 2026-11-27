import {
  BarChart3,
  Clock,
  Copy,
  CornerDownRight,
  ExternalLink,
  Lock,
  MoreHorizontal,
  Pencil,
  Power,
  QrCode,
  Trash2,
  TrendingUp,
} from 'lucide-react';
import { Link, useLocation, useNavigate } from 'react-router';
import { toast } from 'sonner';
import { routes } from '@/app/routes';
import { CopyButton } from '@/components/CopyButton';
import { StampAvatar } from '@/components/StampAvatar';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import { cn } from '@/lib/cn';
import {
  copyToClipboard,
  extractHost,
  formatCompactNumber,
  formatDateTime,
  formatNumber,
  formatRelativeTime,
} from '@/lib/format';
import { buildShortUrl, formatShortLabel } from '@/lib/short-url';
import type { AdminLink } from './api';
import { LinkStatusBadge } from './LinkStatusBadge';

export type LinksViewMode = 'cards' | 'rows';

interface LinkCardProps {
  orgId: number;
  link: AdminLink;
  publicBaseUrl?: string;
  analyticsEnabled?: boolean;
  view: LinksViewMode;
  writable: boolean;
  mutating?: boolean;
  onEdit: (link: AdminLink) => void;
  onShowQRCode: (link: AdminLink) => void;
  onToggleDisable: (link: AdminLink) => void;
  onDelete: (link: AdminLink) => void;
}

export function LinkCard({
  orgId,
  link,
  publicBaseUrl,
  analyticsEnabled = true,
  view,
  writable,
  mutating = false,
  onEdit,
  onShowQRCode,
  onToggleDisable,
  onDelete,
}: LinkCardProps) {
  const navigate = useNavigate();
  const location = useLocation();
  const host = extractHost(link.url);
  const fullShortUrl = buildShortUrl(link.code, publicBaseUrl);
  const shortLabel = formatShortLabel(link.code, publicBaseUrl);
  const statsPath = routes.linkStats(orgId, link.code);
  const fromPath = location.pathname + location.search;
  const isBanned = link.status === 'banned';
  const isDisabled = link.status === 'disabled';
  const compact = view === 'rows';

  async function handleMenuCopy() {
    const ok = await copyToClipboard(fullShortUrl);
    if (ok) toast.success(`已复制 ${fullShortUrl}`);
  }

  return (
    <div
      className={cn(
        'group relative flex flex-col gap-2.5 transition-all sm:flex-row sm:items-center sm:justify-between sm:gap-3',
        compact
          ? 'border-b border-border-soft bg-card px-4 py-2.5 last:border-b-0 hover:bg-paper-warm/45'
          : cn(
              'rounded-2xl border-[1.25px] px-4 py-3.5',
              isBanned
                ? 'border-stamp-red/65 bg-red-50/40 hover:border-stamp-red'
                : isDisabled
                  ? 'border-ink/45 bg-card/80 opacity-85 hover:border-ink/80'
                  : 'border-ink/75 bg-card hover:border-ink hover:shadow-[3px_3px_0_#1F1E1B]',
            ),
      )}
    >
      {/* 悬停左侧荧光黄装订条（卡片视图） */}
      {!compact && !isBanned && (
        <span
          aria-hidden="true"
          className="pointer-events-none absolute inset-y-2.5 left-1 w-1 rounded-full bg-hl-yellow opacity-0 transition-opacity group-hover:opacity-100"
        />
      )}

      <div className="flex min-w-0 flex-1 items-start gap-3 sm:items-center sm:gap-3.5">
        <StampAvatar
          seed={host}
          label={isBanned ? '!' : host}
          size={compact ? 'sm' : 'md'}
          statusDot={isBanned ? 'banned' : link.status === 'active' ? 'active' : undefined}
        />

        <div
          className={cn(
            'min-w-0 flex-1',
            compact ? 'flex flex-col gap-1 sm:flex-row sm:items-center sm:gap-3' : 'space-y-1',
          )}
        >
          <div
            className={cn(
              'flex flex-wrap items-center gap-1.5',
              compact && 'sm:w-72 sm:shrink-0 sm:flex-nowrap',
            )}
          >
            <a
              href={fullShortUrl}
              target="_blank"
              rel="noreferrer"
              title={fullShortUrl}
              className={cn(
                'inline-flex min-w-0 items-center gap-1 font-mono text-sm font-bold whitespace-nowrap transition-colors',
                compact && 'truncate',
                isBanned
                  ? 'text-ink-secondary line-through'
                  : 'text-ink hover:underline hover:decoration-hl-yellow hover:decoration-[3px] hover:underline-offset-4',
              )}
            >
              <span className={cn(compact && 'truncate')}>{shortLabel}</span>
              <ExternalLink className="size-3 shrink-0 text-ink-secondary opacity-0 transition-opacity group-hover:opacity-100" />
            </a>

            <CopyButton
              value={fullShortUrl}
              label={`复制短链 ${link.code}`}
              toastMessage={`已复制 ${fullShortUrl}`}
            />

            <LinkStatusBadge
              status={link.status}
              banReason={link.ban_reason}
              compact
              hideBanReason={compact}
            />

            {link.expires_at && link.status === 'active' && (
              <Tooltip>
                <TooltipTrigger asChild>
                  {compact ? (
                    <span
                      aria-label={`到期时间 ${formatDateTime(link.expires_at)}`}
                      className="inline-flex size-5 shrink-0 items-center justify-center rounded-md bg-hl-peach/65 text-ink"
                    >
                      <Clock className="size-3" />
                    </span>
                  ) : (
                    <span className="hl-swipe-peach inline-flex shrink-0 items-center gap-1 px-1.5 py-0.5 text-[11px] font-medium text-ink">
                      <Clock className="size-3" />
                      定时过期
                    </span>
                  )}
                </TooltipTrigger>
                <TooltipContent>到期时间：{formatDateTime(link.expires_at)}</TooltipContent>
              </Tooltip>
            )}
          </div>

          <div
            className={cn(
              'flex min-w-0 items-center gap-2 text-xs text-ink-secondary',
              compact && 'sm:flex-1',
            )}
          >
            <span className="flex min-w-0 items-center gap-1">
              <CornerDownRight className="size-3 shrink-0 text-ink-secondary" />
              {isBanned ? (
                <span
                  title={link.url}
                  className="min-w-0 truncate text-muted-foreground line-through"
                >
                  {link.url}
                </span>
              ) : (
                <a
                  href={link.url}
                  target="_blank"
                  rel="noreferrer"
                  title={link.url}
                  className="min-w-0 truncate hover:text-ink hover:underline"
                >
                  {link.url}
                </a>
              )}
            </span>

            <div className="flex shrink-0 items-center gap-1.5">
              {!compact && <span className="text-ink/30">·</span>}
              <Tooltip>
                <TooltipTrigger asChild>
                  <span className="whitespace-nowrap text-muted-foreground">
                    {formatRelativeTime(link.created_at)}
                  </span>
                </TooltipTrigger>
                <TooltipContent>创建于 {formatDateTime(link.created_at)}</TooltipContent>
              </Tooltip>
              {link.expires_at && !compact && (
                <>
                  <span className="hidden text-ink/30 md:inline">·</span>
                  <span className="hidden whitespace-nowrap text-muted-foreground md:inline">
                    过期 {formatDateTime(link.expires_at)}
                  </span>
                </>
              )}
            </div>
          </div>
        </div>
      </div>

      <div className="flex shrink-0 items-center justify-end gap-1.5 border-t border-border-soft/70 pt-2 sm:border-t-0 sm:pt-0">
        {analyticsEnabled && link.clicks !== undefined && (
          <Tooltip>
            <TooltipTrigger asChild>
              <Link
                to={statsPath}
                state={{ from: fromPath }}
                className="inline-flex h-8 items-center gap-1.5 rounded-lg border-[1.25px] border-ink/75 bg-hl-mint/55 px-2.5 text-xs font-semibold text-ink transition-all hover:border-ink hover:bg-hl-mint focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink"
              >
                <TrendingUp className="size-3.5 shrink-0 text-ink" />
                <span className="font-mono tabular-nums">{formatCompactNumber(link.clicks)}</span>
                <span className="text-ink-secondary">次点击</span>
                {(link.clicks_today ?? 0) > 0 && (
                  <span className="rounded border border-ink/40 bg-card px-1 py-0.5 font-mono text-[10px] font-bold text-emerald-800 tabular-nums">
                    +{formatCompactNumber(link.clicks_today ?? 0)}
                  </span>
                )}
              </Link>
            </TooltipTrigger>
            <TooltipContent>
              累计 {formatNumber(link.clicks)} 次点击，今日 {formatNumber(link.clicks_today ?? 0)}{' '}
              次（点击查看详情）
            </TooltipContent>
          </Tooltip>
        )}

        <Tooltip>
          <TooltipTrigger asChild>
            <button
              type="button"
              aria-label={`二维码 ${link.code}`}
              onClick={() => onShowQRCode(link)}
              className="inline-flex size-8 items-center justify-center rounded-lg border-[1.25px] border-transparent text-ink-secondary transition-colors hover:border-ink/60 hover:bg-paper-warm hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink cursor-pointer"
            >
              <QrCode className="size-4" />
            </button>
          </TooltipTrigger>
          <TooltipContent>查看二维码</TooltipContent>
        </Tooltip>

        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <button
              type="button"
              aria-label={`操作 ${link.code}`}
              className="inline-flex size-8 items-center justify-center rounded-lg border-[1.25px] border-transparent text-ink-secondary transition-colors hover:border-ink/60 hover:bg-paper-warm hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink cursor-pointer"
            >
              <MoreHorizontal className="size-4" />
            </button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="w-44">
            {analyticsEnabled && (
              <DropdownMenuItem onSelect={() => navigate(statsPath, { state: { from: fromPath } })}>
                <BarChart3 className="size-4 text-ink-secondary" />
                查看点击统计
              </DropdownMenuItem>
            )}
            <DropdownMenuItem onSelect={() => void handleMenuCopy()}>
              <Copy className="size-4 text-ink-secondary" />
              复制完整短链
            </DropdownMenuItem>
            <DropdownMenuItem onSelect={() => onShowQRCode(link)}>
              <QrCode className="size-4 text-ink-secondary" />
              查看二维码
            </DropdownMenuItem>
            {writable && (
              <>
                <DropdownMenuSeparator />
                {isBanned && (
                  <div className="flex items-center gap-1.5 px-2 py-1 text-[11px] font-medium text-stamp-red">
                    <Lock className="size-3" />
                    封禁链接不可修改或删除
                  </div>
                )}
                <DropdownMenuItem disabled={isBanned} onSelect={() => onEdit(link)}>
                  <Pencil className="size-4 text-ink-secondary" />
                  编辑目标网址
                </DropdownMenuItem>
                <DropdownMenuItem
                  disabled={isBanned || mutating}
                  onSelect={() => onToggleDisable(link)}
                >
                  <Power className="size-4 text-ink-secondary" />
                  {link.status === 'disabled' ? '启用短链' : '停用短链'}
                </DropdownMenuItem>
                <DropdownMenuSeparator />
                <DropdownMenuItem
                  disabled={isBanned}
                  onSelect={() => onDelete(link)}
                  className="text-stamp-red focus:bg-red-50 focus:text-stamp-red"
                >
                  <Trash2 className="size-4" />
                  永久删除
                </DropdownMenuItem>
              </>
            )}
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </div>
  );
}
