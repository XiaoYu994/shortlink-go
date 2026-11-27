import { useState } from 'react';
import {
  ArrowLeft,
  BarChart3,
  Bot,
  Compass,
  ExternalLink,
  Globe,
  HelpCircle,
  Info,
  Monitor,
  MousePointerClick,
  Pencil,
  Power,
  QrCode,
  RefreshCw,
  ShieldAlert,
  Smartphone,
  Tablet,
  Terminal,
  Trash2,
  TrendingUp,
  Users,
} from 'lucide-react';
import { Link, useLocation, useNavigate, useParams, useSearchParams } from 'react-router';
import { toast } from 'sonner';
import { useCurrentOrg } from '@/app/org-context';
import { routes } from '@/app/routes';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { CopyButton } from '@/components/CopyButton';
import { CourierCat } from '@/components/CourierCat';
import { EmptyState } from '@/components/EmptyState';
import { ErrorAlert } from '@/components/ErrorAlert';
import { QRCodeDialog } from '@/components/QRCodeDialog';
import { StampAvatar } from '@/components/StampAvatar';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { SegmentedControl } from '@/components/ui/segmented-control';
import { Skeleton } from '@/components/ui/skeleton';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import { useCapabilities } from '@/features/auth/api';
import { canWriteLinks } from '@/features/auth/permissions';
import { getErrorMessage } from '@/lib/api-client';
import { cn } from '@/lib/cn';
import { extractHost, formatDateTime, formatNumber } from '@/lib/format';
import { buildShortUrl, formatShortLabel } from '@/lib/short-url';
import { useDocumentTitle } from '@/lib/use-document-title';
import {
  type StatsRangeDays,
  useDeleteLink,
  useLinkDetail,
  useLinkStats,
  useUpdateLink,
} from './api';
import { AreaTrendChart } from './AreaTrendChart';
import { BreakdownBarList } from './BreakdownBarList';
import {
  formatBanReason,
  formatClientLabel,
  formatDeviceLabel,
  formatReferrerLabel,
} from './link-labels';
import { LinkFormDialog } from './LinkFormDialog';
import { LinkStatusBadge } from './LinkStatusBadge';

const DEVICE_ICONS = {
  desktop: Monitor,
  mobile: Smartphone,
  tablet: Tablet,
  bot: Bot,
  other: HelpCircle,
} as const;

const CLIENT_ICONS = {
  browser: Compass,
  bot: Bot,
  cli: Terminal,
  other: HelpCircle,
} as const;

function parseDaysParam(raw: string | null): StatsRangeDays {
  if (raw === '30') return 30;
  if (raw === '90') return 90;
  return 7;
}

function readReturnFromState(state: unknown): string | null {
  if (
    state &&
    typeof state === 'object' &&
    'from' in state &&
    typeof (state as { from?: unknown }).from === 'string'
  ) {
    const candidate = (state as { from: string }).from;
    if (candidate.startsWith('/orgs/')) return candidate;
  }
  return null;
}

export function LinkStatsPage() {
  const { org } = useCurrentOrg();
  const { code = '' } = useParams();
  const navigate = useNavigate();
  const location = useLocation();
  const [searchParams, setSearchParams] = useSearchParams();

  const capabilitiesQuery = useCapabilities();
  const publicBaseUrl = capabilitiesQuery.data?.public_base_url;
  const analyticsEnabled = capabilitiesQuery.isSuccess && capabilitiesQuery.data.features.analytics;
  const analyticsDisabledByServer =
    capabilitiesQuery.isSuccess && !capabilitiesQuery.data.features.analytics;
  const writable = canWriteLinks(org.role);

  const days = parseDaysParam(searchParams.get('days'));
  const [dimTab, setDimTab] = useState<'devices' | 'clients'>('devices');
  const [qrOpen, setQrOpen] = useState(false);
  const [editOpen, setEditOpen] = useState(false);
  const [deleteOpen, setDeleteOpen] = useState(false);

  useDocumentTitle(`/r/${code} 统计 · ${org.name}`);

  const detailQuery = useLinkDetail(org.id, code);
  const statsQuery = useLinkStats(org.id, code, days, analyticsEnabled);
  const updateLink = useUpdateLink(org.id);
  const deleteLink = useDeleteLink(org.id);

  const backTarget = readReturnFromState(location.state) ?? routes.orgLinks(org.id);
  const fullShortUrl = buildShortUrl(code, publicBaseUrl);
  const shortLabel = formatShortLabel(code, publicBaseUrl);
  const link = detailQuery.data;
  const host = link ? extractHost(link.url) : code;
  const isBanned = link?.status === 'banned';

  function handleChangeDays(nextDays: StatsRangeDays) {
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (nextDays === 7) next.delete('days');
        else next.set('days', String(nextDays));
        return next;
      },
      { replace: true },
    );
  }

  async function handleToggleDisable() {
    if (!link) return;
    const nextDisabled = link.status !== 'disabled';
    try {
      await updateLink.mutateAsync({
        code: link.code,
        payload: { disabled: nextDisabled },
      });
      toast.success(nextDisabled ? `已停用 /r/${link.code}` : `已启用 /r/${link.code}`);
    } catch (err) {
      toast.error(getErrorMessage(err));
    }
  }

  async function handleConfirmDelete() {
    if (!link) return;
    try {
      await deleteLink.mutateAsync(link.code);
      toast.success(`已永久删除 /r/${link.code}`);
      setDeleteOpen(false);
      navigate(backTarget, { replace: true });
    } catch (err) {
      toast.error(getErrorMessage(err));
    }
  }

  return (
    <div className="space-y-6">
      <div className="space-y-4">
        <Link
          to={backTarget}
          className="inline-flex items-center gap-1.5 text-xs font-medium text-ink-secondary transition-colors hover:text-ink"
        >
          <ArrowLeft className="size-3.5" />
          返回短链列表
        </Link>

        {/* 顶部邮票信息卡：集成页内完整闭环操作 */}
        <Card className="border-[1.5px] border-ink ink-shadow-sm">
          <CardContent className="flex flex-col gap-4 p-4 sm:flex-row sm:items-start sm:justify-between">
            <div className="flex min-w-0 flex-1 items-start gap-3.5">
              <StampAvatar
                seed={host}
                label={isBanned ? '!' : host}
                statusDot={isBanned ? 'banned' : link?.status === 'active' ? 'active' : undefined}
              />
              <div className="min-w-0 flex-1 space-y-1.5">
                <div className="flex flex-wrap items-center gap-2">
                  <h1 className="font-mono text-xl font-bold tracking-tight text-ink">
                    {shortLabel}
                  </h1>
                  {link && <LinkStatusBadge status={link.status} banReason={link.ban_reason} />}
                </div>
                {link && (
                  <div className="flex flex-col gap-0.5 text-xs text-ink-secondary sm:flex-row sm:items-center sm:gap-2">
                    <span className="flex min-w-0 items-center gap-1" title={link.url}>
                      <span className="shrink-0">指向</span>
                      {isBanned ? (
                        <span className="min-w-0 truncate font-medium text-ink line-through">
                          {link.url}
                        </span>
                      ) : (
                        <a
                          href={link.url}
                          target="_blank"
                          rel="noreferrer"
                          className="min-w-0 truncate font-medium text-ink hover:underline"
                        >
                          {link.url}
                        </a>
                      )}
                    </span>
                    <span className="shrink-0 whitespace-nowrap text-muted-foreground">
                      <span className="hidden sm:inline">· </span>
                      创建于 {formatDateTime(link.created_at)}
                      {link.expires_at ? ` · 过期 ${formatDateTime(link.expires_at)}` : ''}
                    </span>
                  </div>
                )}
                <div className="flex flex-wrap items-center gap-1.5 pt-1">
                  <CopyButton
                    value={fullShortUrl}
                    variant="pill"
                    size="xs"
                    label="复制短链"
                    toastMessage={`已复制 ${fullShortUrl}`}
                  />
                  <Button
                    variant="outline"
                    size="sm"
                    className="h-7 px-2.5 text-xs"
                    onClick={() => setQrOpen(true)}
                  >
                    <QrCode className="size-3.5" />
                    二维码
                  </Button>
                  {!isBanned && (
                    <a
                      href={fullShortUrl}
                      target="_blank"
                      rel="noreferrer"
                      className="inline-flex h-7 items-center gap-1 rounded-lg border border-transparent px-2 text-xs font-medium text-ink-secondary hover:border-ink/40 hover:bg-paper-warm hover:text-ink"
                    >
                      访问
                      <ExternalLink className="size-3" />
                    </a>
                  )}
                  {writable && link && !isBanned && (
                    <>
                      <Button
                        variant="outline"
                        size="sm"
                        className="h-7 px-2.5 text-xs"
                        onClick={() => setEditOpen(true)}
                      >
                        <Pencil className="size-3" />
                        编辑
                      </Button>
                      <Button
                        variant="outline"
                        size="sm"
                        className="h-7 px-2.5 text-xs"
                        disabled={updateLink.isPending}
                        onClick={() => void handleToggleDisable()}
                      >
                        <Power className="size-3" />
                        {link.status === 'disabled' ? '启用' : '停用'}
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        className="h-7 px-2 text-xs text-stamp-red hover:bg-red-50 hover:text-stamp-red"
                        onClick={() => setDeleteOpen(true)}
                      >
                        <Trash2 className="size-3" />
                        删除
                      </Button>
                    </>
                  )}
                </div>
              </div>
            </div>

            {!analyticsDisabledByServer && !capabilitiesQuery.isError && (
              <div className="flex shrink-0 flex-wrap items-center gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  aria-label="刷新统计"
                  disabled={statsQuery.isFetching}
                  onClick={() => void statsQuery.refetch()}
                >
                  <RefreshCw className={cn('size-3.5', statsQuery.isFetching && 'animate-spin')} />
                  刷新
                </Button>
                <SegmentedControl<StatsRangeDays>
                  ariaLabel="统计时间窗口"
                  size="md"
                  value={days}
                  onChange={handleChangeDays}
                  options={[
                    { value: 7, label: '近 7 天' },
                    { value: 30, label: '近 30 天' },
                    { value: 90, label: '近 90 天' },
                  ]}
                />
              </div>
            )}
          </CardContent>
        </Card>
      </div>

      {/* 异常状态上下文横幅 */}
      {link?.status === 'banned' && (
        <div className="flex flex-col gap-3 rounded-2xl border-[1.5px] border-stamp-red bg-red-50/80 p-4 sm:flex-row sm:items-center sm:justify-between">
          <div className="flex items-start gap-3">
            <CourierCat pose="stamp" className="h-14 w-20 shrink-0" />
            <div className="space-y-1 text-xs text-[#991b1b]">
              <p className="flex items-center gap-1.5 text-sm font-bold">
                <ShieldAlert className="size-4" />
                该短链已被平台安全风控封禁
              </p>
              <p>
                封禁原因：{formatBanReason(link.ban_reason) || '触发安全策略'}
                {link.banned_at ? ` · 封禁时间：${formatDateTime(link.banned_at)}` : ''}
              </p>
              <p>
                封禁期间访客将无法打开目标网页，当前短链暂不可修改或删除；如需申诉恢复，请联系管理员处理。
              </p>
            </div>
          </div>
        </div>
      )}

      {link?.status === 'expired' && writable && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border-[1.25px] border-ink bg-hl-peach/35 px-4 py-3 text-xs text-ink">
          <span>该短链已于 {formatDateTime(link.expires_at)} 过期，当前访客无法访问。</span>
          <Button size="sm" variant="highlight" onClick={() => setEditOpen(true)}>
            延长或取消过期
          </Button>
        </div>
      )}

      {capabilitiesQuery.isError ? (
        <ErrorAlert
          message={`功能状态加载失败：${getErrorMessage(capabilitiesQuery.error)}`}
          onRetry={() => void capabilitiesQuery.refetch()}
        />
      ) : analyticsDisabledByServer ? (
        <EmptyState
          icon={BarChart3}
          illustration={<CourierCat pose="shield" />}
          title="访问统计暂未开启"
          description="当前工作空间尚未开启访问统计功能，如需启用请联系管理员。"
        />
      ) : (
        <>
          {statsQuery.isError && (
            <ErrorAlert
              message={`加载统计失败：${getErrorMessage(statsQuery.error)}`}
              onRetry={() => void statsQuery.refetch()}
            />
          )}

          {statsQuery.isPending ? (
            <div className="space-y-4">
              <div className="grid gap-4 sm:grid-cols-3">
                <Skeleton className="h-24 rounded-2xl" />
                <Skeleton className="h-24 rounded-2xl" />
                <Skeleton className="h-24 rounded-2xl" />
              </div>
              <Skeleton className="h-72 rounded-2xl" />
            </div>
          ) : statsQuery.data ? (
            <>
              <div className="grid gap-4 sm:grid-cols-3">
                <Card>
                  <CardHeader className="flex-row items-center justify-between pb-1">
                    <CardDescription className="text-xs font-medium text-ink-secondary">
                      历史总点击
                    </CardDescription>
                    <span className="flex size-7 items-center justify-center rounded-lg border border-ink/50 bg-hl-yellow/75 text-ink">
                      <MousePointerClick className="size-3.5" />
                    </span>
                  </CardHeader>
                  <CardContent>
                    <span className="hl-swipe-yellow inline-block px-1 font-mono text-2xl font-bold tabular-nums text-ink">
                      {formatNumber(statsQuery.data.total_clicks)}
                    </span>
                  </CardContent>
                </Card>
                <Card>
                  <CardHeader className="flex-row items-center justify-between pb-1">
                    <CardDescription className="text-xs font-medium text-ink-secondary">
                      近 {days} 天点击
                    </CardDescription>
                    <span className="flex size-7 items-center justify-center rounded-lg border border-ink/50 bg-hl-mint/80 text-ink">
                      <TrendingUp className="size-3.5" />
                    </span>
                  </CardHeader>
                  <CardContent>
                    <span className="hl-swipe-mint inline-block px-1 font-mono text-2xl font-bold tabular-nums text-ink">
                      {formatNumber(statsQuery.data.range_clicks)}
                    </span>
                  </CardContent>
                </Card>
                <Card>
                  <CardHeader className="flex-row items-center justify-between pb-1">
                    <CardDescription className="flex items-center gap-1 text-xs font-medium text-ink-secondary">
                      近 {days} 天访客（日去重累计）
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <button
                            type="button"
                            aria-label="独立访客口径说明"
                            className="inline-flex text-ink-secondary hover:text-ink cursor-help"
                          >
                            <Info className="size-3.5" />
                          </button>
                        </TooltipTrigger>
                        <TooltipContent>
                          每日按独立访客匿名去重后累计；同一访客在不同日期访问会分别计入。
                        </TooltipContent>
                      </Tooltip>
                    </CardDescription>
                    <span className="flex size-7 items-center justify-center rounded-lg border border-ink/50 bg-hl-peach/70 text-ink">
                      <Users className="size-3.5" />
                    </span>
                  </CardHeader>
                  <CardContent>
                    <span className="hl-swipe-peach inline-block px-1 font-mono text-2xl font-bold tabular-nums text-ink">
                      {formatNumber(statsQuery.data.unique_visitors)}
                    </span>
                  </CardContent>
                </Card>
              </div>

              <Card>
                <CardHeader className="flex-row items-center justify-between gap-4">
                  <CardTitle>访问趋势</CardTitle>
                  <CourierCat pose="analytics" className="hidden h-11 w-14 shrink-0 sm:block" />
                </CardHeader>
                <CardContent>
                  <AreaTrendChart
                    shortUrl={fullShortUrl}
                    clicks={statsQuery.data.clicks}
                    visitors={statsQuery.data.daily_visitors}
                  />
                </CardContent>
              </Card>

              <div className="grid gap-6 lg:grid-cols-2">
                <Card>
                  <CardHeader>
                    <CardTitle>主要来源网站</CardTitle>
                  </CardHeader>
                  <CardContent>
                    <BreakdownBarList
                      tone="mint"
                      emptyText="该时间范围内暂无来源记录。"
                      items={statsQuery.data.referrers.map((r) => ({
                        key: r.referrer,
                        label: formatReferrerLabel(r.referrer),
                        count: r.clicks,
                        icon: Globe,
                      }))}
                    />
                  </CardContent>
                </Card>

                <Card>
                  <CardHeader className="flex-row items-center justify-between gap-2">
                    <CardTitle>访问环境分布</CardTitle>
                    <SegmentedControl<'devices' | 'clients'>
                      ariaLabel="切换访问环境维度"
                      value={dimTab}
                      onChange={setDimTab}
                      options={[
                        { value: 'devices', label: '设备' },
                        { value: 'clients', label: '客户端' },
                      ]}
                    />
                  </CardHeader>
                  <CardContent>
                    {dimTab === 'devices' ? (
                      <BreakdownBarList
                        tone="peach"
                        emptyText="该时间范围内暂无设备分布数据。"
                        items={statsQuery.data.devices
                          .slice()
                          .sort((a, b) => b.clicks - a.clicks)
                          .map((d) => ({
                            key: d.device,
                            label: formatDeviceLabel(d.device),
                            count: d.clicks,
                            icon: DEVICE_ICONS[d.device as keyof typeof DEVICE_ICONS] ?? HelpCircle,
                          }))}
                      />
                    ) : (
                      <BreakdownBarList
                        tone="yellow"
                        emptyText="该时间范围内暂无客户端分布数据。"
                        items={statsQuery.data.clients
                          .slice()
                          .sort((a, b) => b.clicks - a.clicks)
                          .map((c) => ({
                            key: c.client,
                            label: formatClientLabel(c.client),
                            count: c.clicks,
                            icon: CLIENT_ICONS[c.client as keyof typeof CLIENT_ICONS] ?? HelpCircle,
                          }))}
                      />
                    )}
                  </CardContent>
                </Card>
              </div>
            </>
          ) : null}
        </>
      )}

      <QRCodeDialog
        open={qrOpen}
        onOpenChange={setQrOpen}
        code={code}
        shortUrl={fullShortUrl}
        targetUrl={link?.url}
      />

      {link && (
        <LinkFormDialog
          orgId={org.id}
          open={editOpen}
          onOpenChange={setEditOpen}
          editingLink={link}
        />
      )}

      <ConfirmDialog
        open={deleteOpen}
        onOpenChange={setDeleteOpen}
        title={`永久删除短链 /r/${code}？`}
        description="删除后该短链及历史统计将一并清除且无法恢复，且不返还本月已使用的创建配额。"
        confirmLabel="确认删除"
        pending={deleteLink.isPending}
        onConfirm={handleConfirmDelete}
      />
    </div>
  );
}
