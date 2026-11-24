import { useState } from 'react';
import {
  ArrowLeft,
  Bot,
  Compass,
  ExternalLink,
  Globe,
  HelpCircle,
  Info,
  Monitor,
  MousePointerClick,
  QrCode,
  Smartphone,
  Tablet,
  Terminal,
  TrendingUp,
  Users,
} from 'lucide-react';
import { Link, useParams } from 'react-router';
import { useCurrentOrg } from '@/app/org-context';
import { routes } from '@/app/routes';
import { CopyButton } from '@/components/CopyButton';
import { ErrorAlert } from '@/components/ErrorAlert';
import { MonogramAvatar } from '@/components/MonogramAvatar';
import { QRCodeDialog } from '@/components/QRCodeDialog';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { SegmentedControl } from '@/components/ui/segmented-control';
import { Skeleton } from '@/components/ui/skeleton';
import { usePublicBaseUrl } from '@/features/auth/api';
import { getErrorMessage } from '@/lib/api-client';
import { extractHost, formatDateTime, formatNumber } from '@/lib/format';
import { buildShortUrl, formatShortLabel } from '@/lib/short-url';
import { useDocumentTitle } from '@/lib/use-document-title';
import { useLinkDetail, useLinkStats } from './api';
import { AreaTrendChart } from './AreaTrendChart';
import { BreakdownBarList } from './BreakdownBarList';
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

export function LinkStatsPage() {
  const { org } = useCurrentOrg();
  const { code = '' } = useParams();
  const publicBaseUrl = usePublicBaseUrl();
  const [days, setDays] = useState<7 | 30>(7);
  const [dimTab, setDimTab] = useState<'devices' | 'clients'>('devices');
  const [qrOpen, setQrOpen] = useState(false);

  useDocumentTitle(`/r/${code} 统计 · ${org.name}`);

  const detailQuery = useLinkDetail(org.id, code);
  const statsQuery = useLinkStats(org.id, code, days);

  const fullShortUrl = buildShortUrl(code, publicBaseUrl);
  const shortLabel = formatShortLabel(code, publicBaseUrl);
  const host = detailQuery.data ? extractHost(detailQuery.data.url) : code;

  return (
    <div className="space-y-6">
      <div className="space-y-4">
        <Link
          to={routes.orgLinks(org.id)}
          className="inline-flex items-center gap-1.5 text-xs font-medium text-zinc-500 transition-colors hover:text-zinc-950"
        >
          <ArrowLeft className="size-3.5" />
          返回短链列表
        </Link>

        <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
          <div className="flex min-w-0 items-start gap-3.5">
            <MonogramAvatar seed={host} label={host} />
            <div className="min-w-0 space-y-1">
              <div className="flex flex-wrap items-center gap-2">
                <h1 className="font-mono text-xl font-semibold tracking-tight text-zinc-950">
                  {shortLabel}
                </h1>
                {detailQuery.data && (
                  <LinkStatusBadge
                    status={detailQuery.data.status}
                    banReason={detailQuery.data.ban_reason}
                  />
                )}
                <CopyButton
                  value={fullShortUrl}
                  variant="pill"
                  label="复制"
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
                <a
                  href={fullShortUrl}
                  target="_blank"
                  rel="noreferrer"
                  className="inline-flex h-7 items-center gap-1 rounded-md px-2 text-xs font-medium text-brand hover:bg-indigo-50"
                >
                  访问
                  <ExternalLink className="size-3" />
                </a>
              </div>
              {detailQuery.data && (
                <p className="truncate text-xs text-zinc-500" title={detailQuery.data.url}>
                  指向 <span className="text-zinc-700">{detailQuery.data.url}</span> · 创建于{' '}
                  {formatDateTime(detailQuery.data.created_at)}
                </p>
              )}
            </div>
          </div>

          <SegmentedControl<7 | 30>
            ariaLabel="统计时间窗口"
            size="md"
            className="shrink-0"
            value={days}
            onChange={setDays}
            options={[
              { value: 7, label: '近 7 天' },
              { value: 30, label: '近 30 天' },
            ]}
          />
        </div>
      </div>

      {statsQuery.isError && (
        <ErrorAlert
          message={`加载统计失败：${getErrorMessage(statsQuery.error)}`}
          onRetry={() => void statsQuery.refetch()}
        />
      )}

      {statsQuery.isPending ? (
        <div className="space-y-4">
          <div className="grid gap-4 sm:grid-cols-3">
            <Skeleton className="h-24 rounded-xl" />
            <Skeleton className="h-24 rounded-xl" />
            <Skeleton className="h-24 rounded-xl" />
          </div>
          <Skeleton className="h-72 rounded-xl" />
        </div>
      ) : statsQuery.data ? (
        <>
          <div className="grid gap-4 sm:grid-cols-3">
            <Card>
              <CardHeader className="flex-row items-center justify-between pb-1">
                <CardDescription className="text-xs font-medium">历史总点击</CardDescription>
                <span className="flex size-7 items-center justify-center rounded-lg bg-indigo-50 text-brand">
                  <MousePointerClick className="size-3.5" />
                </span>
              </CardHeader>
              <CardContent>
                <div className="font-mono text-2xl font-bold tabular-nums text-zinc-950">
                  {formatNumber(statsQuery.data.total_clicks)}
                </div>
              </CardContent>
            </Card>
            <Card>
              <CardHeader className="flex-row items-center justify-between pb-1">
                <CardDescription className="text-xs font-medium">近 {days} 天点击</CardDescription>
                <span className="flex size-7 items-center justify-center rounded-lg bg-sky-50 text-sky-600">
                  <TrendingUp className="size-3.5" />
                </span>
              </CardHeader>
              <CardContent>
                <div className="font-mono text-2xl font-bold tabular-nums text-zinc-950">
                  {formatNumber(statsQuery.data.range_clicks)}
                </div>
              </CardContent>
            </Card>
            <Card>
              <CardHeader className="flex-row items-center justify-between pb-1">
                <CardDescription className="flex items-center gap-1 text-xs font-medium">
                  近 {days} 天访客（日去重累计）
                  <span
                    title="按每个自然日（UTC）独立去重后相加，不记录原始 IP；同一访客跨天访问会分别计入。"
                    className="inline-flex text-zinc-400 cursor-help"
                  >
                    <Info className="size-3.5" />
                  </span>
                </CardDescription>
                <span className="flex size-7 items-center justify-center rounded-lg bg-emerald-50 text-emerald-600">
                  <Users className="size-3.5" />
                </span>
              </CardHeader>
              <CardContent>
                <div className="font-mono text-2xl font-bold tabular-nums text-zinc-950">
                  {formatNumber(statsQuery.data.unique_visitors)}
                </div>
              </CardContent>
            </Card>
          </div>

          <Card>
            <CardHeader>
              <CardTitle>访问趋势</CardTitle>
              <CardDescription>
                鼠标移到曲线上可查看每天的点击数与访客数（点击约 1 秒内异步更新）。
              </CardDescription>
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
                <CardDescription>
                  按点击量降序展示前 10 个引流来源（直接访问记为 direct）。
                </CardDescription>
              </CardHeader>
              <CardContent>
                <BreakdownBarList
                  tone="indigo"
                  emptyText="该时间范围内暂无来源记录。"
                  items={statsQuery.data.referrers.map((r) => ({
                    key: r.referrer,
                    label: r.referrer,
                    count: r.clicks,
                    icon: Globe,
                  }))}
                />
              </CardContent>
            </Card>

            <Card>
              <CardHeader className="flex-row items-center justify-between gap-2">
                <div>
                  <CardTitle>访问环境分布</CardTitle>
                  <CardDescription>按终端设备或客户端类型拆分。</CardDescription>
                </div>
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
                    tone="sky"
                    emptyText="该时间范围内暂无设备分布数据。"
                    items={statsQuery.data.devices
                      .slice()
                      .sort((a, b) => b.clicks - a.clicks)
                      .map((d) => ({
                        key: d.device,
                        label: d.device,
                        count: d.clicks,
                        icon: DEVICE_ICONS[d.device as keyof typeof DEVICE_ICONS] ?? HelpCircle,
                      }))}
                  />
                ) : (
                  <BreakdownBarList
                    tone="emerald"
                    emptyText="该时间范围内暂无客户端分布数据。"
                    items={statsQuery.data.clients
                      .slice()
                      .sort((a, b) => b.clicks - a.clicks)
                      .map((c) => ({
                        key: c.client,
                        label: c.client,
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

      <QRCodeDialog
        open={qrOpen}
        onOpenChange={setQrOpen}
        code={code}
        shortUrl={fullShortUrl}
        targetUrl={detailQuery.data?.url}
      />
    </div>
  );
}
