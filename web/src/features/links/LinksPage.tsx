import { useEffect, useMemo, useState } from 'react';
import {
  ArrowRight,
  Eye,
  LayoutList,
  Link2,
  Plus,
  RefreshCw,
  Rows3,
  Search,
  X,
} from 'lucide-react';
import { useNavigate } from 'react-router';
import { toast } from 'sonner';
import { useCurrentOrg } from '@/app/org-context';
import { routes } from '@/app/routes';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { EmptyState } from '@/components/EmptyState';
import { ErrorAlert } from '@/components/ErrorAlert';
import { PageHeader } from '@/components/PageHeader';
import { QRCodeDialog } from '@/components/QRCodeDialog';
import { Button } from '@/components/ui/button';
import { Input, Select } from '@/components/ui/input';
import { SegmentedControl } from '@/components/ui/segmented-control';
import { Skeleton } from '@/components/ui/skeleton';
import { usePublicBaseUrl } from '@/features/auth/api';
import { canWriteLinks } from '@/features/auth/permissions';
import { getErrorMessage } from '@/lib/api-client';
import { cn } from '@/lib/cn';
import { formatNumber } from '@/lib/format';
import { buildShortUrl, isExactShortCode } from '@/lib/short-url';
import { useDocumentTitle } from '@/lib/use-document-title';
import {
  type AdminLink,
  type LinkSort,
  useDeleteLink,
  useInfiniteLinks,
  useUpdateLink,
} from './api';
import { LinkCard, type LinksViewMode } from './LinkCard';
import { LinkFormDialog } from './LinkFormDialog';

export function LinksPage() {
  const { org } = useCurrentOrg();
  const navigate = useNavigate();
  const publicBaseUrl = usePublicBaseUrl();
  const writable = canWriteLinks(org.role);

  useDocumentTitle(`短链管理 · ${org.name}`);

  const [sort, setSort] = useState<LinkSort>('id');
  const [view, setView] = useState<LinksViewMode>('cards');
  const [search, setSearch] = useState('');
  const [formOpen, setFormOpen] = useState(false);
  const [editingLink, setEditingLink] = useState<AdminLink | null>(null);
  const [qrLink, setQrLink] = useState<AdminLink | null>(null);
  const [deletingLink, setDeletingLink] = useState<AdminLink | null>(null);

  const linksQuery = useInfiniteLinks(org.id, sort);
  const updateLink = useUpdateLink(org.id);
  const deleteLink = useDeleteLink(org.id);

  // 非输入态下按键盘 C 键快速打开「创建短链」弹窗。
  useEffect(() => {
    if (!writable) return;
    function onKeyDown(e: KeyboardEvent) {
      if (e.metaKey || e.ctrlKey || e.altKey || e.key.toLowerCase() !== 'c') return;
      if (formOpen || deletingLink !== null || qrLink !== null) return;
      const target = e.target as HTMLElement | null;
      if (
        target &&
        (target.tagName === 'INPUT' ||
          target.tagName === 'TEXTAREA' ||
          target.tagName === 'SELECT' ||
          target.isContentEditable)
      ) {
        return;
      }
      e.preventDefault();
      setEditingLink(null);
      setFormOpen(true);
    }
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [writable, formOpen, deletingLink, qrLink]);

  const allLinks = useMemo(
    () => linksQuery.data?.pages.flatMap((page) => page.items) ?? [],
    [linksQuery.data],
  );

  const trimmedSearch = search.trim();
  const filteredLinks = useMemo(() => {
    const keyword = trimmedSearch.toLowerCase();
    if (!keyword) return allLinks;
    return allLinks.filter(
      (item) =>
        item.code.toLowerCase().includes(keyword) || item.url.toLowerCase().includes(keyword),
    );
  }, [allLinks, trimmedSearch]);

  const summary = useMemo(() => {
    let active = 0;
    let totalClicks = 0;
    let todayClicks = 0;
    for (const item of allLinks) {
      if (item.status === 'active') active += 1;
      totalClicks += item.clicks;
      todayClicks += item.clicks_today;
    }
    return { total: allLinks.length, active, totalClicks, todayClicks };
  }, [allLinks]);

  const canDirectLookup = isExactShortCode(trimmedSearch);

  async function handleToggleDisable(link: AdminLink) {
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
    if (!deletingLink) return;
    try {
      await deleteLink.mutateAsync(deletingLink.code);
      toast.success(`已永久删除 /r/${deletingLink.code}`);
      setDeletingLink(null);
    } catch (err) {
      toast.error(getErrorMessage(err));
    }
  }

  return (
    <div className="space-y-6">
      <PageHeader
        title="短链管理"
        description={`管理「${org.name}」的短链资产、实时点击表现与访问状态。`}
        actions={
          <>
            <Button
              variant="outline"
              size="sm"
              aria-label="刷新列表"
              disabled={linksQuery.isFetching}
              onClick={() => void linksQuery.refetch()}
            >
              <RefreshCw className={cn('size-3.5', linksQuery.isFetching && 'animate-spin')} />
              刷新
            </Button>
            {writable && (
              <Button
                size="sm"
                onClick={() => {
                  setEditingLink(null);
                  setFormOpen(true);
                }}
              >
                <Plus className="size-4" />
                创建短链
                <kbd className="ml-1 hidden rounded bg-white/15 px-1.5 py-0.5 font-mono text-[10px] text-zinc-200 sm:inline">
                  C
                </kbd>
              </Button>
            )}
          </>
        }
      />

      {!writable && (
        <div className="flex items-center gap-2 rounded-xl border border-zinc-200 bg-zinc-50/80 px-3.5 py-2.5 text-xs text-zinc-600">
          <Eye className="size-4 shrink-0 text-zinc-400" />
          当前你在该组织的角色为只读观察者（Viewer），可以浏览短链列表与点击统计，无法创建、编辑或删除。
        </div>
      )}

      {/* 诚实口径概览条：全局配额来自后端真实计数，点击统计明确标示为“已加载短链汇总” */}
      {allLinks.length > 0 && (
        <div className="grid grid-cols-3 divide-x divide-zinc-200/80 rounded-xl border border-zinc-200/90 bg-zinc-50/50">
          <div className="px-4 py-3">
            <div className="text-xs text-zinc-500">本月创建配额</div>
            <div className="mt-1 flex items-baseline gap-1.5">
              <span className="font-mono text-lg font-semibold tabular-nums text-zinc-950">
                {formatNumber(org.link_usage)}
              </span>
              <span className="font-mono text-xs text-zinc-400">
                / {formatNumber(org.link_quota)}
              </span>
            </div>
          </div>
          <div className="px-4 py-3">
            <div className="flex items-center gap-1.5 text-xs text-zinc-500">
              已加载短链
              {linksQuery.hasNextPage && (
                <span className="rounded bg-zinc-200/70 px-1 py-0.5 text-[10px] text-zinc-600">
                  含更多页
                </span>
              )}
            </div>
            <div className="mt-1 flex items-baseline gap-2">
              <span className="font-mono text-lg font-semibold tabular-nums text-zinc-950">
                {formatNumber(summary.total)}
              </span>
              <span className="text-xs text-zinc-400">{summary.active} 条活跃</span>
            </div>
          </div>
          <div className="px-4 py-3">
            <div className="text-xs text-zinc-500">
              {linksQuery.hasNextPage ? '已加载短链点击' : '累计点击'}
            </div>
            <div className="mt-1 flex items-baseline gap-2">
              <span className="font-mono text-lg font-semibold tabular-nums text-zinc-950">
                {formatNumber(summary.totalClicks)}
              </span>
              {summary.todayClicks > 0 && (
                <span className="font-mono text-xs text-emerald-600 tabular-nums">
                  今日 +{formatNumber(summary.todayClicks)}
                </span>
              )}
            </div>
          </div>
        </div>
      )}

      {/* 工具栏：筛选已加载项或输入 8 位短码精确直达 + 排序 + 视图切换 */}
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="relative w-full sm:max-w-sm">
          <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-zinc-400" />
          <Input
            aria-label="筛选短链"
            placeholder="筛选已加载短链，或输入 8 位短码直达…"
            className="pl-9 pr-8"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
          {search && (
            <button
              type="button"
              aria-label="清空搜索"
              onClick={() => setSearch('')}
              className="absolute right-2.5 top-1/2 -translate-y-1/2 rounded p-0.5 text-zinc-400 hover:bg-zinc-100 hover:text-zinc-700 cursor-pointer"
            >
              <X className="size-3.5" />
            </button>
          )}
        </div>

        <div className="flex items-center gap-2">
          <Select
            aria-label="排序方式"
            value={sort}
            onChange={(e) => setSort(e.target.value as LinkSort)}
          >
            <option value="id">按最新创建</option>
            <option value="clicks">按累计点击</option>
            <option value="clicks_today">按今日点击</option>
          </Select>

          <SegmentedControl<LinksViewMode>
            ariaLabel="切换列表视图"
            value={view}
            onChange={setView}
            className="h-9"
            options={[
              {
                value: 'cards',
                label: <LayoutList className="size-3.5" />,
                title: '卡片视图',
                ariaLabel: '卡片视图',
              },
              {
                value: 'rows',
                label: <Rows3 className="size-3.5" />,
                title: '紧凑列表',
                ariaLabel: '紧凑列表',
              },
            ]}
          />
        </div>
      </div>

      {linksQuery.isError && (
        <ErrorAlert
          message={`加载短链失败：${getErrorMessage(linksQuery.error)}`}
          onRetry={() => void linksQuery.refetch()}
        />
      )}

      {linksQuery.isPending ? (
        <div className="space-y-3">
          <Skeleton className="h-[74px] w-full rounded-xl" />
          <Skeleton className="h-[74px] w-full rounded-xl" />
          <Skeleton className="h-[74px] w-full rounded-xl" />
        </div>
      ) : allLinks.length === 0 ? (
        <EmptyState
          icon={Link2}
          title="当前组织还没有短链"
          description={
            writable
              ? '创建你的第一条短链，提交后立即生效并自动生成分享二维码与访问统计。'
              : '组织成员创建短链后，你可以在这里浏览状态与访问统计。'
          }
          action={
            writable ? (
              <Button
                onClick={() => {
                  setEditingLink(null);
                  setFormOpen(true);
                }}
              >
                <Plus className="size-4" />
                创建第一条短链
              </Button>
            ) : undefined
          }
        />
      ) : filteredLinks.length === 0 ? (
        <EmptyState
          icon={Search}
          title="已加载列表中未找到匹配项"
          description={
            linksQuery.hasNextPage
              ? `当前仅在已加载的 ${allLinks.length} 条记录中筛选，目标短链可能在后续分页中。`
              : `全部 ${allLinks.length} 条记录中未找到包含「${trimmedSearch}」的短链。`
          }
          action={
            <div className="flex flex-wrap items-center justify-center gap-2">
              {canDirectLookup && (
                <Button size="sm" onClick={() => navigate(routes.linkStats(org.id, trimmedSearch))}>
                  直接查询短码 /r/{trimmedSearch}
                  <ArrowRight className="size-3.5" />
                </Button>
              )}
              <Button variant="outline" size="sm" onClick={() => setSearch('')}>
                清空筛选
              </Button>
            </div>
          }
        />
      ) : (
        <div className="space-y-4">
          <div
            className={cn(
              view === 'cards'
                ? 'space-y-2.5'
                : 'overflow-hidden rounded-xl border border-zinc-200/90 bg-white',
            )}
          >
            {filteredLinks.map((link) => (
              <LinkCard
                key={link.code}
                orgId={org.id}
                link={link}
                publicBaseUrl={publicBaseUrl}
                view={view}
                writable={writable}
                mutating={updateLink.isPending}
                onEdit={(item) => {
                  setEditingLink(item);
                  setFormOpen(true);
                }}
                onShowQRCode={(item) => setQrLink(item)}
                onToggleDisable={(item) => void handleToggleDisable(item)}
                onDelete={(item) => setDeletingLink(item)}
              />
            ))}
          </div>

          {linksQuery.hasNextPage && (
            <div className="flex justify-center pt-1">
              <Button
                variant="outline"
                size="sm"
                disabled={linksQuery.isFetchingNextPage}
                onClick={() => void linksQuery.fetchNextPage()}
              >
                {linksQuery.isFetchingNextPage ? '加载更多…' : '加载更多短链'}
              </Button>
            </div>
          )}
        </div>
      )}

      <LinkFormDialog
        orgId={org.id}
        open={formOpen}
        onOpenChange={setFormOpen}
        editingLink={editingLink}
      />

      {qrLink && (
        <QRCodeDialog
          open
          onOpenChange={(open) => {
            if (!open) setQrLink(null);
          }}
          code={qrLink.code}
          shortUrl={buildShortUrl(qrLink.code, publicBaseUrl)}
          targetUrl={qrLink.url}
        />
      )}

      <ConfirmDialog
        open={deletingLink !== null}
        onOpenChange={(open) => {
          if (!open) setDeletingLink(null);
        }}
        title={`永久删除短链 /r/${deletingLink?.code ?? ''}？`}
        description="删除后该短链及历史统计将一并清除且无法恢复，且不返还本月已使用的创建配额。"
        confirmLabel="确认删除"
        pending={deleteLink.isPending}
        onConfirm={handleConfirmDelete}
      />
    </div>
  );
}
