import { type FormEvent, useEffect, useMemo, useRef, useState } from 'react';
import {
  ArrowRight,
  CornerDownLeft,
  Eye,
  LayoutList,
  Link2,
  Plus,
  RefreshCw,
  Rows3,
  Search,
  Send,
  Sparkles,
  X,
} from 'lucide-react';
import { useNavigate, useSearchParams } from 'react-router';
import { toast } from 'sonner';
import { useCurrentOrg } from '@/app/org-context';
import { routes } from '@/app/routes';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { CopyButton } from '@/components/CopyButton';
import { CourierCat } from '@/components/CourierCat';
import { EmptyState } from '@/components/EmptyState';
import { ErrorAlert } from '@/components/ErrorAlert';
import { PageHeader } from '@/components/PageHeader';
import { QRCodeDialog } from '@/components/QRCodeDialog';
import { Button } from '@/components/ui/button';
import { Input, Select } from '@/components/ui/input';
import { SegmentedControl } from '@/components/ui/segmented-control';
import { Skeleton } from '@/components/ui/skeleton';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import { useAnalyticsEnabled, usePublicBaseUrl } from '@/features/auth/api';
import { canWriteLinks } from '@/features/auth/permissions';
import { getErrorMessage } from '@/lib/api-client';
import { cn } from '@/lib/cn';
import { MAX_LINK_SEARCH_BYTES } from '@/lib/constants';
import { formatNumber } from '@/lib/format';
import { buildShortUrl, isExactShortCode } from '@/lib/short-url';
import { useDocumentTitle } from '@/lib/use-document-title';
import {
  type AdminLink,
  type LinkSort,
  type LinkStatusFilter,
  useCreateLink,
  useDeleteLink,
  useInfiniteLinks,
  useUpdateLink,
} from './api';
import { LinkCard, type LinksViewMode } from './LinkCard';
import { LinkFormDialog } from './LinkFormDialog';

const utf8Encoder = new TextEncoder();
const VIEW_STORAGE_KEY = 'ori:links-view';

const STATUS_TABS: ReadonlyArray<{ value: LinkStatusFilter; label: string }> = [
  { value: '', label: '全部' },
  { value: 'active', label: '活跃' },
  { value: 'disabled', label: '已停用' },
  { value: 'expired', label: '已过期' },
  { value: 'banned', label: '已封禁' },
];

type QuickPreset = 'never' | '24h' | '7d';

function parseStatusParam(raw: string | null): LinkStatusFilter {
  if (raw === 'active' || raw === 'disabled' || raw === 'expired' || raw === 'banned') {
    return raw;
  }
  return '';
}

function parseSortParam(raw: string | null): LinkSort {
  if (raw === 'clicks' || raw === 'clicks_today') return raw;
  return 'id';
}

function readStoredView(): LinksViewMode {
  try {
    const val = window.localStorage.getItem(VIEW_STORAGE_KEY);
    return val === 'rows' ? 'rows' : 'cards';
  } catch {
    return 'cards';
  }
}

function persistView(view: LinksViewMode): void {
  try {
    window.localStorage.setItem(VIEW_STORAGE_KEY, view);
  } catch {
    // 忽略无痕模式等存储受限环境
  }
}

export function LinksPage() {
  const { org } = useCurrentOrg();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const searchInputRef = useRef<HTMLInputElement>(null);

  const publicBaseUrl = usePublicBaseUrl();
  const analyticsEnabled = useAnalyticsEnabled();
  const writable = canWriteLinks(org.role);
  const quotaUnavailable = org.link_quota === 0 || org.link_usage >= org.link_quota;

  useDocumentTitle(`短链管理 · ${org.name}`);

  const statusFilter = parseStatusParam(searchParams.get('status'));
  const rawSort = parseSortParam(searchParams.get('sort'));
  const effectiveSort: LinkSort = analyticsEnabled ? rawSort : 'id';
  const urlQuery = searchParams.get('q') ?? '';

  const [search, setSearch] = useState(urlQuery);
  const debounceTimerRef = useRef<number | null>(null);
  const [view, setView] = useState<LinksViewMode>(readStoredView);

  // 顶部极速缩短框状态
  const [quickUrl, setQuickUrl] = useState('');
  const [quickPreset, setQuickPreset] = useState<QuickPreset>('never');
  const [quickError, setQuickError] = useState<string | null>(null);
  const [quickCreated, setQuickCreated] = useState<AdminLink | null>(null);

  // 弹窗与行级操作状态
  const [formOpen, setFormOpen] = useState(false);
  const [editingLink, setEditingLink] = useState<AdminLink | null>(null);
  const [qrLink, setQrLink] = useState<AdminLink | null>(null);
  const [deletingLink, setDeletingLink] = useState<AdminLink | null>(null);
  const [mutatingCode, setMutatingCode] = useState<string | null>(null);

  function updateQueryParams(updates: { status?: LinkStatusFilter; sort?: LinkSort; q?: string }) {
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (updates.status !== undefined) {
          if (updates.status) next.set('status', updates.status);
          else next.delete('status');
        }
        if (updates.sort !== undefined) {
          if (updates.sort !== 'id') next.set('sort', updates.sort);
          else next.delete('sort');
        }
        if (updates.q !== undefined) {
          if (updates.q) next.set('q', updates.q);
          else next.delete('q');
        }
        return next;
      },
      { replace: true },
    );
  }

  function handleSearchChange(nextValue: string) {
    setSearch(nextValue);
    if (debounceTimerRef.current !== null) {
      window.clearTimeout(debounceTimerRef.current);
      debounceTimerRef.current = null;
    }
    const trimmed = nextValue.trim();
    if (utf8Encoder.encode(trimmed).length > MAX_LINK_SEARCH_BYTES) return;
    debounceTimerRef.current = window.setTimeout(() => {
      debounceTimerRef.current = null;
      updateQueryParams({ q: trimmed });
    }, 300);
  }

  useEffect(() => {
    return () => {
      if (debounceTimerRef.current !== null) {
        window.clearTimeout(debounceTimerRef.current);
      }
    };
  }, []);

  const linksQuery = useInfiniteLinks(org.id, effectiveSort, statusFilter, urlQuery);
  const createLink = useCreateLink(org.id);
  const updateLink = useUpdateLink(org.id);
  const deleteLink = useDeleteLink(org.id);

  // 非输入态下按 C 快速打开「创建短链」弹窗，按 / 聚焦搜索框。
  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.metaKey || e.ctrlKey || e.altKey) return;
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
      if (e.key === '/') {
        e.preventDefault();
        searchInputRef.current?.focus();
        return;
      }
      if (
        writable &&
        !quotaUnavailable &&
        e.key.toLowerCase() === 'c' &&
        !formOpen &&
        deletingLink === null &&
        qrLink === null
      ) {
        e.preventDefault();
        setEditingLink(null);
        setFormOpen(true);
      }
    }
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [writable, quotaUnavailable, formOpen, deletingLink, qrLink]);

  const allLinks = useMemo(() => {
    const pages = linksQuery.data?.pages;
    if (!pages) return [];
    const seen = new Set<string>();
    const deduped: AdminLink[] = [];
    for (const page of pages) {
      for (const item of page.items) {
        if (seen.has(item.code)) continue;
        seen.add(item.code);
        deduped.push(item);
      }
    }
    return deduped;
  }, [linksQuery.data]);

  const trimmedSearch = search.trim();
  const searchTooLong = utf8Encoder.encode(trimmedSearch).length > MAX_LINK_SEARCH_BYTES;
  const hasActiveFilters = statusFilter !== '' || urlQuery !== '';

  function handleClearFilters() {
    if (debounceTimerRef.current !== null) {
      window.clearTimeout(debounceTimerRef.current);
      debounceTimerRef.current = null;
    }
    setSearch('');
    updateQueryParams({ status: '', q: '' });
  }

  function handleChangeView(nextView: LinksViewMode) {
    setView(nextView);
    persistView(nextView);
  }

  const summary = useMemo(() => {
    let active = 0;
    let banned = 0;
    let todayClicks = 0;
    for (const item of allLinks) {
      if (item.status === 'active') active += 1;
      if (item.status === 'banned') banned += 1;
      todayClicks += item.clicks_today ?? 0;
    }
    return { total: allLinks.length, active, banned, todayClicks };
  }, [allLinks]);

  const canDirectLookup = isExactShortCode(trimmedSearch);

  async function handleQuickShorten(e: FormEvent) {
    e.preventDefault();
    const raw = quickUrl.trim();
    if (!raw) {
      setQuickError('请输入需要缩短的 http 或 https 网址');
      return;
    }
    setQuickError(null);
    const expiresAt =
      quickPreset === '24h'
        ? new Date(Date.now() + 24 * 3600_000).toISOString()
        : quickPreset === '7d'
          ? new Date(Date.now() + 7 * 24 * 3600_000).toISOString()
          : undefined;
    try {
      const created = await createLink.mutateAsync({
        url: raw,
        ...(expiresAt ? { expires_at: expiresAt } : {}),
      });
      setQuickUrl('');
      setQuickCreated(created);
      toast.success(`纸飞机已折叠！生成 /r/${created.code}`);
    } catch (err) {
      setQuickError(getErrorMessage(err));
    }
  }

  async function handleToggleDisable(link: AdminLink) {
    const nextDisabled = link.status !== 'disabled';
    setMutatingCode(link.code);
    try {
      await updateLink.mutateAsync({
        code: link.code,
        payload: { disabled: nextDisabled },
      });
      toast.success(nextDisabled ? `已停用 /r/${link.code}` : `已启用 /r/${link.code}`);
    } catch (err) {
      toast.error(getErrorMessage(err));
    } finally {
      setMutatingCode(null);
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

  const quickShortUrl = quickCreated ? buildShortUrl(quickCreated.code, publicBaseUrl) : '';
  const showQuickHttpsHint =
    Boolean(quickUrl.trim()) &&
    !/^https?:\/\//i.test(quickUrl.trim()) &&
    /^[^\s/]+\.[^\s]+/.test(quickUrl.trim());

  const createBtn = writable ? (
    quotaUnavailable ? (
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="inline-flex">
            <Button size="sm" variant="highlight" disabled>
              <Plus className="size-4" />
              创建短链
            </Button>
          </span>
        </TooltipTrigger>
        <TooltipContent>
          {org.link_quota === 0
            ? '当前组织未开放创建额度'
            : '本月创建配额已用完，下月 1 日自动重置'}
        </TooltipContent>
      </Tooltip>
    ) : (
      <Button
        size="sm"
        variant="highlight"
        onClick={() => {
          setEditingLink(null);
          setFormOpen(true);
        }}
      >
        <Plus className="size-4" />
        创建短链
        <kbd className="ml-0.5 hidden rounded border border-ink/40 bg-card/70 px-1.5 py-0.5 font-mono text-[10px] text-ink sm:inline">
          C
        </kbd>
      </Button>
    )
  ) : null;

  return (
    <div className="space-y-5">
      <PageHeader
        title="短链管理"
        actions={
          <div className="flex flex-wrap items-center gap-2">
            {allLinks.length > 0 && (
              <div className="hidden items-center gap-1.5 rounded-xl border border-dashed border-ink/45 bg-card/80 px-2.5 py-1 text-xs text-ink-secondary md:inline-flex">
                <span>
                  活跃 <strong className="font-mono text-ink">{summary.active}</strong>
                </span>
                {analyticsEnabled && (
                  <>
                    <span className="text-ink/30">·</span>
                    <span>
                      今日点击{' '}
                      <strong className="font-mono text-emerald-700">
                        +{formatNumber(summary.todayClicks)}
                      </strong>
                    </span>
                  </>
                )}
                {summary.banned > 0 && (
                  <>
                    <span className="text-ink/30">·</span>
                    <button
                      type="button"
                      onClick={() => updateQueryParams({ status: 'banned' })}
                      className="rounded-md bg-red-100 px-1.5 py-0.5 font-medium text-[#991b1b] hover:bg-red-200/80 cursor-pointer"
                    >
                      风控拦截 {summary.banned}
                    </button>
                  </>
                )}
              </div>
            )}

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
            {createBtn}
          </div>
        }
      />

      {!writable && (
        <div className="flex items-center gap-2 rounded-xl border-[1.25px] border-ink/45 bg-paper-warm/70 px-3.5 py-2.5 text-xs text-ink-secondary">
          <Eye className="size-4 shrink-0 text-ink" />
          当前为只读成员权限，可以浏览短链列表与访问统计，无法新增、编辑或删除。
        </div>
      )}

      {/* 顶部一体化「极速缩短路由框」 */}
      {writable && (
        <div className="space-y-2">
          <form
            onSubmit={handleQuickShorten}
            noValidate
            className={cn(
              'flex flex-col gap-2 rounded-2xl border-[1.5px] border-ink bg-card p-2 ink-shadow-sm sm:flex-row sm:items-center',
              quotaUnavailable && 'opacity-70',
            )}
          >
            <div className="flex min-w-0 flex-1 items-center gap-2 pl-2">
              <Send className="size-4 shrink-0 text-ink-secondary" />
              <input
                id="quick-shorten-url"
                name="quickUrl"
                type="url"
                aria-label="极速缩短长链接"
                disabled={quotaUnavailable || createLink.isPending}
                placeholder={
                  quotaUnavailable
                    ? '本月创建配额已不可用，暂无法创建新短链'
                    : '粘贴任意长链接，回车瞬间折叠成短链…'
                }
                value={quickUrl}
                onChange={(e) => {
                  setQuickUrl(e.target.value);
                  if (quickError) setQuickError(null);
                }}
                className="h-9 min-w-0 flex-1 bg-transparent text-sm text-ink placeholder:text-muted-foreground focus:outline-none disabled:cursor-not-allowed"
              />
            </div>

            <div className="flex flex-wrap items-center justify-between gap-2 border-t border-border-soft pt-2 sm:border-t-0 sm:pt-0">
              <div
                role="group"
                aria-label="快捷有效期"
                className="inline-flex items-center rounded-lg border border-ink/40 bg-paper-warm/70 p-0.5 text-[11px]"
              >
                {(
                  [
                    { id: 'never', label: '永不过期' },
                    { id: '24h', label: '24h' },
                    { id: '7d', label: '7天' },
                  ] as const
                ).map((preset) => (
                  <button
                    key={preset.id}
                    type="button"
                    aria-pressed={quickPreset === preset.id}
                    onClick={() => setQuickPreset(preset.id)}
                    className={cn(
                      'rounded-md px-2 py-1 font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink cursor-pointer',
                      quickPreset === preset.id
                        ? 'border border-ink bg-card text-ink'
                        : 'text-ink-secondary hover:text-ink',
                    )}
                  >
                    {preset.label}
                  </button>
                ))}
              </div>

              <Button
                type="submit"
                size="sm"
                variant="highlight"
                disabled={quotaUnavailable || createLink.isPending}
              >
                {createLink.isPending ? '折叠中…' : '立即折叠'}
                <CornerDownLeft className="size-3.5" />
              </Button>
            </div>
          </form>

          {showQuickHttpsHint && (
            <div className="flex items-center justify-between gap-2 rounded-xl border border-ink/35 bg-hl-yellow/35 px-3 py-1.5 text-xs text-ink">
              <span>网址似乎缺少协议头，是否补全为 https:// ？</span>
              <button
                type="button"
                onClick={() => setQuickUrl(`https://${quickUrl.trim()}`)}
                className="inline-flex items-center gap-1 rounded-md border border-ink bg-card px-2 py-0.5 font-medium hover:bg-hl-yellow cursor-pointer"
              >
                <Sparkles className="size-3" />
                一键补全
              </button>
            </div>
          )}

          {quickError && <ErrorAlert compact message={quickError} />}

          {quickCreated && (
            <div className="flex flex-wrap items-center justify-between gap-2 rounded-xl border-[1.25px] border-ink bg-hl-mint/45 px-3.5 py-2 text-xs">
              <div className="flex min-w-0 items-center gap-2">
                <CourierCat pose="toast" />
                <span className="font-medium text-ink">刚刚折好：</span>
                <a
                  href={quickShortUrl}
                  target="_blank"
                  rel="noreferrer"
                  className="truncate font-mono font-bold text-ink underline"
                >
                  {quickShortUrl}
                </a>
              </div>
              <div className="flex items-center gap-1.5">
                <CopyButton
                  value={quickShortUrl}
                  variant="pill"
                  size="xs"
                  label="复制短链"
                  toastMessage={`已复制 ${quickShortUrl}`}
                />
                <Button
                  size="sm"
                  variant="outline"
                  className="h-7 px-2 text-xs"
                  onClick={() => setQrLink(quickCreated)}
                >
                  二维码
                </Button>
                <button
                  type="button"
                  aria-label="收起结果提示"
                  onClick={() => setQuickCreated(null)}
                  className="inline-flex size-7 items-center justify-center rounded-md text-ink-secondary hover:bg-card/80 hover:text-ink cursor-pointer"
                >
                  <X className="size-3.5" />
                </button>
              </div>
            </div>
          )}
        </div>
      )}

      {/* 工具栏：状态荧光胶囊 + 搜索（支持 / 聚焦）+ 排序 + 视图切换 */}
      <div className="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
        <div role="group" aria-label="状态筛选" className="flex flex-wrap items-center gap-1">
          {STATUS_TABS.map((tab) => {
            const active = statusFilter === tab.value;
            return (
              <button
                key={tab.value || 'all'}
                type="button"
                aria-pressed={active}
                onClick={() => updateQueryParams({ status: tab.value })}
                className={cn(
                  'rounded-lg px-2.5 py-1.5 text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink cursor-pointer',
                  active
                    ? 'hl-swipe-mint font-bold text-ink'
                    : 'text-ink-secondary hover:bg-paper-warm hover:text-ink',
                )}
              >
                {tab.label}
              </button>
            );
          })}
        </div>

        <div className="flex flex-wrap items-center gap-2">
          <div className="relative w-full sm:w-64">
            <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-ink-secondary" />
            <Input
              ref={searchInputRef}
              id="links-search"
              name="q"
              aria-label="搜索短链"
              placeholder="搜索短码或网址…"
              className="pl-9 pr-9"
              value={search}
              onChange={(e) => handleSearchChange(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  if (debounceTimerRef.current !== null) {
                    window.clearTimeout(debounceTimerRef.current);
                    debounceTimerRef.current = null;
                  }
                  const trimmed = search.trim();
                  if (utf8Encoder.encode(trimmed).length <= MAX_LINK_SEARCH_BYTES) {
                    updateQueryParams({ q: trimmed });
                  }
                }
              }}
            />
            {search && (
              <button
                type="button"
                aria-label="清空搜索"
                onClick={() => {
                  if (debounceTimerRef.current !== null) {
                    window.clearTimeout(debounceTimerRef.current);
                    debounceTimerRef.current = null;
                  }
                  setSearch('');
                  updateQueryParams({ q: '' });
                }}
                className="absolute right-1.5 top-1/2 inline-flex size-7 -translate-y-1/2 items-center justify-center rounded-md text-ink-secondary hover:bg-paper-warm hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink cursor-pointer"
              >
                <X className="size-3.5" />
              </button>
            )}
          </div>

          <Select
            id="links-sort"
            name="sort"
            aria-label="排序方式"
            value={effectiveSort}
            onChange={(e) => updateQueryParams({ sort: e.target.value as LinkSort })}
          >
            <option value="id">按最新创建</option>
            {analyticsEnabled && <option value="clicks">按累计点击</option>}
            {analyticsEnabled && <option value="clicks_today">按今日点击</option>}
          </Select>

          <SegmentedControl<LinksViewMode>
            ariaLabel="切换列表视图"
            value={view}
            onChange={handleChangeView}
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

      {/* 8 位短码精确直达提示条（无论列表是否已有结果均可直接跳转） */}
      {analyticsEnabled && canDirectLookup && (
        <div className="flex items-center justify-between gap-3 rounded-xl border-[1.25px] border-ink/75 bg-hl-yellow/40 px-3.5 py-2 text-xs">
          <span className="text-ink">
            检测到 8 位完整短码 <code className="font-mono font-bold">/r/{trimmedSearch}</code>
            ，可直接查看单链统计
          </span>
          <Button
            size="sm"
            variant="outline"
            className="h-7 px-2.5 text-xs"
            onClick={() =>
              navigate(routes.linkStats(org.id, trimmedSearch), {
                state: { from: `${routes.orgLinks(org.id)}?${searchParams.toString()}` },
              })
            }
          >
            直达 /r/{trimmedSearch}
            <ArrowRight className="size-3.5" />
          </Button>
        </div>
      )}

      {searchTooLong && <ErrorAlert compact message="搜索关键词过长，请精简后再试" />}

      {linksQuery.isError && (
        <ErrorAlert
          message={`加载短链失败：${getErrorMessage(linksQuery.error)}`}
          onRetry={() => void linksQuery.refetch()}
        />
      )}

      {linksQuery.isPending ? (
        <div className="space-y-3">
          <Skeleton className="h-[74px] w-full rounded-2xl" />
          <Skeleton className="h-[74px] w-full rounded-2xl" />
          <Skeleton className="h-[74px] w-full rounded-2xl" />
        </div>
      ) : allLinks.length === 0 && !hasActiveFilters ? (
        <EmptyState
          icon={Link2}
          illustration={<CourierCat pose="empty" />}
          title="当前组织还没有短链"
          description={
            writable
              ? '在上方粘贴长链接即可折叠生成短链，也可点击下方按钮创建并获取分享二维码。'
              : '组织成员创建短链后，你可以在这里浏览状态与访问统计。'
          }
          action={
            writable && !quotaUnavailable ? (
              <Button
                variant="highlight"
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
      ) : allLinks.length === 0 ? (
        <EmptyState
          icon={Search}
          illustration={<CourierCat pose="shield" />}
          title="未找到匹配的短链"
          description={
            urlQuery
              ? `当前筛选条件下没有匹配「${urlQuery}」的短链。`
              : '当前状态筛选下没有符合条件的短链。'
          }
          action={
            <div className="flex flex-wrap items-center justify-center gap-2">
              {analyticsEnabled && canDirectLookup && (
                <Button
                  size="sm"
                  variant="highlight"
                  onClick={() => navigate(routes.linkStats(org.id, trimmedSearch))}
                >
                  直接查询短码 /r/{trimmedSearch}
                  <ArrowRight className="size-3.5" />
                </Button>
              )}
              <Button variant="outline" size="sm" onClick={handleClearFilters}>
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
                : 'overflow-hidden rounded-2xl border-[1.25px] border-ink bg-card',
            )}
          >
            {allLinks.map((link) => (
              <LinkCard
                key={link.code}
                orgId={org.id}
                link={link}
                publicBaseUrl={publicBaseUrl}
                analyticsEnabled={analyticsEnabled}
                view={view}
                writable={writable}
                mutating={mutatingCode === link.code}
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
