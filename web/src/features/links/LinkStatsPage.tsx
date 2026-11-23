import { useState } from 'react';
import {
  ArrowLeft,
  Bot,
  Compass,
  ExternalLink,
  Globe,
  HelpCircle,
  Monitor,
  MousePointerClick,
  Smartphone,
  Tablet,
  Terminal,
  TrendingUp,
  Users,
} from 'lucide-react';
import { Link, useParams } from 'react-router';
import { useCurrentOrg } from '@/app/org-context';
import { CopyButton } from '@/components/CopyButton';
import { MonogramAvatar } from '@/components/MonogramAvatar';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { getErrorMessage } from '@/lib/api-client';
import { cn } from '@/lib/cn';
import { extractHost, formatDateTime, formatNumber } from '@/lib/format';
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
  const [days, setDays] = useState<7 | 30>(7);
  const [dimTab, setDimTab] = useState<'devices' | 'clients'>('devices');

  const detailQuery = useLinkDetail(org.id, code);
  const statsQuery = useLinkStats(org.id, code, days);

  const fullShortUrl = `${window.location.origin}/r/${code}`;
  const host = detailQuery.data ? extractHost(detailQuery.data.url) : code;

  return (
    <div className="space-y-6">
      {/* 顶部面包屑 + 短链信息头 */}
      <div className="space-y-4">
        <Link
          to={`/orgs/${org.id}/links`}
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
                  /r/{code}
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
                <a
                  href={`/r/${code}`}
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

          <div className="inline-flex shrink-0 rounded-lg border border-zinc-200 bg-zinc-50 p-0.5 text-xs">
            {([7, 30] as const).map((windowDays) => (
              <button
                key={windowDays}
                type="button"
                className={cn(
                  'rounded-md px-3 py-1.5 font-medium transition-all cursor-pointer',
                  days === windowDays
                    ? 'bg-white text-zinc-950 shadow-2xs ring-1 ring-zinc-200/80'
                    : 'text-zinc-500 hover:text-zinc-900',
                )}
                onClick={() => setDays(windowDays)}
              >
                近 {windowDays} 天
              </button>
            ))}
          </div>
        </div>
      </div>

      {statsQuery.isError && (
        <div className="flex items-center justify-between rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
          <span>加载统计失败：{getErrorMessage(statsQuery.error)}</span>
          <Button variant="outline" size="sm" onClick={() => void statsQuery.refetch()}>
            重试
          </Button>
        </div>
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
          {/* 3 个核心 KPI 卡片 */}
          <div className="grid gap-4 sm:grid-cols-3">
            <Card>
              <CardHeader className="flex-row items-center justify-between pb-1">
                <CardDescription className="text-xs font-medium">终身累计点击</CardDescription>
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
                <CardDescription className="text-xs font-medium">按天去重访客之和</CardDescription>
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

          {/* 主面积趋势折线图 */}
          <Card>
            <CardHeader>
              <CardTitle>访问趋势</CardTitle>
              <CardDescription>鼠标移到曲线上可查看每日点击数与独立访客数。</CardDescription>
            </CardHeader>
            <CardContent>
              <AreaTrendChart
                code={code}
                clicks={statsQuery.data.clicks}
                visitors={statsQuery.data.daily_visitors}
              />
            </CardContent>
          </Card>

          {/* 来源 + 设备/客户端明细（Dub.co 行内占比条） */}
          <div className="grid gap-6 lg:grid-cols-2">
            <Card>
              <CardHeader>
                <CardTitle>Top 来源域名（Referer）</CardTitle>
                <CardDescription>按点击量降序展示前 10 个流量来源。</CardDescription>
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
                <div className="inline-flex rounded-lg border border-zinc-200 bg-zinc-50 p-0.5 text-xs">
                  <button
                    type="button"
                    onClick={() => setDimTab('devices')}
                    className={cn(
                      'rounded-md px-2.5 py-1 font-medium transition-all cursor-pointer',
                      dimTab === 'devices'
                        ? 'bg-white text-zinc-950 shadow-2xs ring-1 ring-zinc-200/80'
                        : 'text-zinc-500 hover:text-zinc-900',
                    )}
                  >
                    设备
                  </button>
                  <button
                    type="button"
                    onClick={() => setDimTab('clients')}
                    className={cn(
                      'rounded-md px-2.5 py-1 font-medium transition-all cursor-pointer',
                      dimTab === 'clients'
                        ? 'bg-white text-zinc-950 shadow-2xs ring-1 ring-zinc-200/80'
                        : 'text-zinc-500 hover:text-zinc-900',
                    )}
                  >
                    客户端
                  </button>
                </div>
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
                        icon:
                          DEVICE_ICONS[d.device as keyof typeof DEVICE_ICONS] ?? HelpCircle,
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
                        icon:
                          CLIENT_ICONS[c.client as keyof typeof CLIENT_ICONS] ?? HelpCircle,
                      }))}
                  />
                )}
              </CardContent>
            </Card>
          </div>
        </>
      ) : null}
    </div>
  );
}
