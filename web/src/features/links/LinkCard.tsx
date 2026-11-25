import {
  BarChart3,
  Clock,
  Copy,
  CornerDownRight,
  ExternalLink,
  Lock,
  MoreHorizontal,
  MousePointerClick,
  Pencil,
  Power,
  QrCode,
  Trash2,
} from 'lucide-react';
import { Link, useNavigate } from 'react-router';
import { toast } from 'sonner';
import { routes } from '@/app/routes';
import { CopyButton } from '@/components/CopyButton';
import { MonogramAvatar } from '@/components/MonogramAvatar';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
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
  const host = extractHost(link.url);
  const fullShortUrl = buildShortUrl(link.code, publicBaseUrl);
  const shortLabel = formatShortLabel(link.code, publicBaseUrl);
  const statsPath = routes.linkStats(orgId, link.code);
  const isBanned = link.status === 'banned';
  const compact = view === 'rows';

  async function handleMenuCopy() {
    const ok = await copyToClipboard(fullShortUrl);
    if (ok) toast.success(`已复制 ${fullShortUrl}`);
  }

  return (
    <div
      className={cn(
        'group flex items-center justify-between gap-3 bg-white transition-all',
        compact
          ? 'border-b border-zinc-200/80 px-4 py-2.5 last:border-b-0 hover:bg-zinc-50/70'
          : 'rounded-xl border border-zinc-200/90 px-4 py-3.5 shadow-[0_1px_2px_rgba(0,0,0,0.02)] hover:border-zinc-300 hover:shadow-[0_2px_8px_rgba(0,0,0,0.04)]',
      )}
    >
      <div className="flex min-w-0 flex-1 items-center gap-3.5">
        <MonogramAvatar seed={host} label={host} size={compact ? 'sm' : 'md'} />

        <div
          className={cn(
            'min-w-0 flex-1',
            compact ? 'flex flex-col gap-0.5 sm:flex-row sm:items-center sm:gap-3' : 'space-y-1',
          )}
        >
          <div className="flex shrink-0 items-center gap-1.5">
            <a
              href={fullShortUrl}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 font-mono text-sm font-semibold text-zinc-950 transition-colors hover:text-brand"
            >
              {shortLabel}
              <ExternalLink className="size-3 text-zinc-400 opacity-0 transition-opacity group-hover:opacity-100" />
            </a>
            <CopyButton
              value={fullShortUrl}
              label={`复制短链 ${link.code}`}
              toastMessage={`已复制 ${fullShortUrl}`}
            />
            <LinkStatusBadge status={link.status} banReason={link.ban_reason} compact />
          </div>

          <div className="flex min-w-0 items-center gap-1.5 text-xs text-zinc-500">
            <CornerDownRight className="size-3 shrink-0 text-zinc-400" />
            {isBanned ? (
              <span title={link.url} className="truncate text-zinc-400 line-through">
                {link.url}
              </span>
            ) : (
              <a
                href={link.url}
                target="_blank"
                rel="noreferrer"
                title={link.url}
                className="truncate hover:text-zinc-800 hover:underline"
              >
                {link.url}
              </a>
            )}
            <span className="hidden shrink-0 text-zinc-300 sm:inline">·</span>
            <span
              title={`创建于 ${formatDateTime(link.created_at)}`}
              className="hidden shrink-0 whitespace-nowrap text-zinc-400 sm:inline"
            >
              {formatRelativeTime(link.created_at)}
            </span>
            {link.expires_at && !compact && (
              <>
                <span className="hidden shrink-0 text-zinc-300 md:inline">·</span>
                <span
                  title={`过期时间 ${formatDateTime(link.expires_at)}`}
                  className="hidden shrink-0 items-center gap-1 whitespace-nowrap text-zinc-400 md:inline-flex"
                >
                  <Clock className="size-3" />
                  {formatDateTime(link.expires_at)}
                </span>
              </>
            )}
          </div>
        </div>
      </div>

      <div className="flex shrink-0 items-center gap-2">
        {analyticsEnabled && link.clicks !== undefined && (
          <Link
            to={statsPath}
            title={`累计 ${formatNumber(link.clicks)} 次点击，今日 ${formatNumber(link.clicks_today ?? 0)} 次`}
            className="inline-flex h-7 items-center gap-1.5 rounded-lg border border-zinc-200/90 bg-zinc-50/70 px-2.5 text-xs font-medium text-zinc-700 transition-colors hover:border-indigo-200 hover:bg-indigo-50/70 hover:text-brand"
          >
            <MousePointerClick className="size-3.5 text-zinc-400" />
            <span className="font-mono tabular-nums">{formatCompactNumber(link.clicks)}</span>
            <span className="hidden text-zinc-500 sm:inline">次点击</span>
            {(link.clicks_today ?? 0) > 0 && (
              <span className="rounded bg-emerald-100/80 px-1 py-0.5 font-mono text-[10px] font-semibold text-emerald-700 tabular-nums">
                +{formatCompactNumber(link.clicks_today ?? 0)}
              </span>
            )}
          </Link>
        )}

        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <button
              type="button"
              aria-label={`操作 ${link.code}`}
              className="inline-flex size-7 items-center justify-center rounded-lg border border-transparent text-zinc-400 transition-colors hover:border-zinc-200 hover:bg-zinc-100 hover:text-zinc-800 cursor-pointer"
            >
              <MoreHorizontal className="size-4" />
            </button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="w-44">
            {analyticsEnabled && (
              <DropdownMenuItem onSelect={() => navigate(statsPath)}>
                <BarChart3 className="size-4 text-zinc-500" />
                查看点击统计
              </DropdownMenuItem>
            )}
            <DropdownMenuItem onSelect={() => void handleMenuCopy()}>
              <Copy className="size-4 text-zinc-500" />
              复制完整短链
            </DropdownMenuItem>
            <DropdownMenuItem onSelect={() => onShowQRCode(link)}>
              <QrCode className="size-4 text-zinc-500" />
              查看二维码
            </DropdownMenuItem>
            {writable && (
              <>
                <DropdownMenuSeparator />
                {isBanned && (
                  <div className="flex items-center gap-1.5 px-2 py-1 text-[11px] text-red-600">
                    <Lock className="size-3" />
                    封禁链接不可修改或删除
                  </div>
                )}
                <DropdownMenuItem disabled={isBanned} onSelect={() => onEdit(link)}>
                  <Pencil className="size-4 text-zinc-500" />
                  编辑目标 URL
                </DropdownMenuItem>
                <DropdownMenuItem
                  disabled={isBanned || mutating}
                  onSelect={() => onToggleDisable(link)}
                >
                  <Power className="size-4 text-zinc-500" />
                  {link.status === 'disabled' ? '启用短链' : '停用短链'}
                </DropdownMenuItem>
                <DropdownMenuSeparator />
                <DropdownMenuItem
                  disabled={isBanned}
                  onSelect={() => onDelete(link)}
                  className="text-destructive focus:bg-red-50 focus:text-destructive"
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
