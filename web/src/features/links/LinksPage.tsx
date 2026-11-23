import { useEffect, useMemo, useState } from 'react';
import {
  LayoutList,
  Link2,
  Plus,
  RefreshCw,
  Rows3,
  Search,
  X,
} from 'lucide-react';
import { toast } from 'sonner';
import { useCurrentOrg } from '@/app/org-context';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { EmptyState } from '@/components/EmptyState';
import { PageHeader } from '@/components/PageHeader';
import { Button } from '@/components/ui/button';
import { Input, Select } from '@/components/ui/input';
import { Skeleton } from '@/components/ui/skeleton';
import { canWriteLinks } from '@/features/auth/permissions';
import { getErrorMessage } from '@/lib/api-client';
import { cn } from '@/lib/cn';
import { formatNumber } from '@/lib/format';
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
  const writable = canWriteLinks(org.role);

  const [sort, setSort] = useState<LinkSort>('id');
  const [view, setView] = useState<LinksViewMode>('cards');
  const [search, setSearch] = useState('');
  const [formOpen, setFormOpen] = useState(false);
  const [editingLink, setEditingLink] = useState<AdminLink | null>(null);
  const [deletingLink, setDeletingLink] = useState<AdminLink | null>(null);

  const linksQuery = useInfiniteLinks(org.id, sort);
  const updateLink = useUpdateLink(org.id);
  const deleteLink = useDeleteLink(org.id);

  // 对标 Dub.co / Linear：非输入态下按键盘 C 键快速弹出「创建短链」弹窗。
  useEffect(() => {
    if (!writable) return;
    function onKeyDown(e: KeyboardEvent) {
      if (e.metaKey || e.ctrlKey || e.altKey || e.key.toLowerCase() !== 'c') return;
      if (formOpen || deletingLink !== null) return;
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
  }, [writable, formOpen, deletingLink]);

  const allLinks = useMemo(
    () => linksQuery.data?.pages.flatMap((page) => page.items) ?? [],
    [linksQuery.data],
  );

  const filteredLinks = useMemo(() => {
    const keyword = search.trim().toLowerCase();
    if (!keyword) return allLinks;
    return allLinks.filter(
      (item) =>
        item.code.toLowerCase().includes(keyword) || item.url.toLowerCase().includes(keyword),
    );
  }, [allLinks, search]);

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
        description={`管理「${org.name}」的短链资产、实时点击表现与生命周期状态。`}
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

      {/* 顶部紧凑概览条（仅在已有短链数据时展示，避免空列表显得空洞） */}
      {allLinks.length > 0 && (
        <div className="grid grid-cols-3 divide-x divide-zinc-200/80 rounded-xl border border-zinc-200/90 bg-zinc-50/50">
          <div className="px-4 py-3">
            <div className="text-xs text-zinc-500">已加载短链</div>
            <div className="mt-1 flex items-baseline gap-2">
              <span className="font-mono text-lg font-semibold tabular-nums text-zinc-950">
                {formatNumber(summary.total)}
              </span>
              <span className="text-xs text-zinc-400">{summary.active} 条活跃</span>
            </div>
          </div>
          <div className="px-4 py-3">
            <div className="text-xs text-zinc-500">今日点击</div>
            <div className="mt-1 font-mono text-lg font-semibold tabular-nums text-zinc-950">
              {formatNumber(summary.todayClicks)}
            </div>
          </div>
          <div className="px-4 py-3">
            <div className="text-xs text-zinc-500">累计总点击</div>
            <div className="mt-1 font-mono text-lg font-semibold tabular-nums text-zinc-950">
              {formatNumber(summary.totalClicks)}
            </div>
          </div>
        </div>
      )}

      {/* 工具栏：搜索 + 排序 + 卡片/紧凑行切换 */}
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="relative w-full sm:max-w-sm">
          <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-zinc-400" />
          <Input
            aria-label="筛选短链"
            placeholder="搜索短码或目标 URL…"
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

          <div
            role="group"
            aria-label="切换列表视图"
            className="inline-flex h-9 items-center rounded-md border border-zinc-200 bg-zinc-50/70 p-0.5"
          >
            <button
              type="button"
              title="卡片视图"
              aria-label="卡片视图"
              aria-pressed={view === 'cards'}
              onClick={() => setView('cards')}
              className={cn(
                'inline-flex h-7 items-center justify-center rounded px-2 text-xs transition-all cursor-pointer',
                view === 'cards'
                  ? 'bg-white text-zinc-950 shadow-2xs ring-1 ring-zinc-200/80'
                  : 'text-zinc-500 hover:text-zinc-900',
              )}
            >
              <LayoutList className="size-3.5" />
            </button>
            <button
              type="button"
              title="紧凑列表"
              aria-label="紧凑列表"
              aria-pressed={view === 'rows'}
              onClick={() => setView('rows')}
              className={cn(
                'inline-flex h-7 items-center justify-center rounded px-2 text-xs transition-all cursor-pointer',
                view === 'rows'
                  ? 'bg-white text-zinc-950 shadow-2xs ring-1 ring-zinc-200/80'
                  : 'text-zinc-500 hover:text-zinc-900',
              )}
            >
              <Rows3 className="size-3.5" />
            </button>
          </div>
        </div>
      </div>

      {linksQuery.isError && (
        <div className="flex items-center justify-between rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
          <span>加载短链失败：{getErrorMessage(linksQuery.error)}</span>
          <Button variant="outline" size="sm" onClick={() => void linksQuery.refetch()}>
            重试
          </Button>
        </div>
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
              ? '创建你的第一条短链，系统会自动写穿 Redis 正缓存并在后台异步做 URL 风控扫描。'
              : '当前角色为只读视图，联系组织管理员创建短链后即可在这里查看统计。'
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
          title="没有匹配的短链"
          description={`已加载的 ${allLinks.length} 条短链中未找到包含「${search}」的记录。`}
          action={
            <Button variant="outline" onClick={() => setSearch('')}>
              清空筛选
            </Button>
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
                view={view}
                writable={writable}
                mutating={updateLink.isPending}
                onEdit={(item) => {
                  setEditingLink(item);
                  setFormOpen(true);
                }}
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

      <ConfirmDialog
        open={deletingLink !== null}
        onOpenChange={(open) => {
          if (!open) setDeletingLink(null);
        }}
        title={`永久删除短链 /r/${deletingLink?.code ?? ''}？`}
        description="硬删除会同时清除该短链的历史点击统计且无法恢复，删除操作不返还本月创建配额。"
        confirmLabel="确认删除"
        pending={deleteLink.isPending}
        onConfirm={handleConfirmDelete}
      />
    </div>
  );
}
