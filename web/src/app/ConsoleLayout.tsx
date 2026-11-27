import { useState } from 'react';
import {
  Check,
  ChevronsUpDown,
  KeyRound,
  Link2,
  Lock,
  LogOut,
  Menu,
  Plus,
  ScrollText,
  Users,
  Webhook,
} from 'lucide-react';
import { NavLink, Outlet, useMatch, useNavigate } from 'react-router';
import { toast } from 'sonner';
import { BrandLogo } from '@/components/BrandLogo';
import { StampAvatar } from '@/components/StampAvatar';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { Sheet, SheetContent, SheetDescription, SheetTitle } from '@/components/ui/sheet';
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip';
import { useLogout, useWebhooksEnabled } from '@/features/auth/api';
import {
  canManageAPIKeys,
  canManageWebhooks,
  canReadAuditLogs,
  ROLE_LABELS,
} from '@/features/auth/permissions';
import { setLastOrgId } from '@/features/orgs/api';
import { CreateOrgDialog } from '@/features/orgs/CreateOrgDialog';
import { getErrorMessage } from '@/lib/api-client';
import { cn } from '@/lib/cn';
import { formatNumber } from '@/lib/format';
import { useCurrentOrg } from './org-context';
import { type OrgSection, ROUTE_PATTERNS, routes } from './routes';

interface NavEntry {
  to: string;
  label: string;
  icon: typeof Link2;
  locked: boolean;
}

export function ConsoleLayout() {
  const { user, org, organizations } = useCurrentOrg();
  const navigate = useNavigate();
  const isMembersRoute = Boolean(useMatch(ROUTE_PATTERNS.orgMembers));
  const isAPIKeysRoute = Boolean(useMatch(ROUTE_PATTERNS.orgAPIKeys));
  const isWebhooksRoute = Boolean(useMatch(ROUTE_PATTERNS.orgWebhooks));
  const isAuditLogsRoute = Boolean(useMatch(ROUTE_PATTERNS.orgAuditLogs));
  const logout = useLogout();
  const [createOrgOpen, setCreateOrgOpen] = useState(false);
  const [mobileMenuOpen, setMobileMenuOpen] = useState(false);

  const webhooksEnabled = useWebhooksEnabled();
  const keysLocked = !canManageAPIKeys(org.role);
  const webhooksLocked = !canManageWebhooks(org.role);
  const auditLocked = !canReadAuditLogs(org.role);

  const coreNav: NavEntry[] = [
    { to: routes.orgLinks(org.id), label: '短链管理', icon: Link2, locked: false },
  ];

  const governanceNav: NavEntry[] = [
    { to: routes.orgMembers(org.id), label: '成员与邀请', icon: Users, locked: false },
    { to: routes.orgAPIKeys(org.id), label: 'API Keys', icon: KeyRound, locked: keysLocked },
    ...(webhooksEnabled
      ? [
          {
            to: routes.orgWebhooks(org.id),
            label: 'Webhooks',
            icon: Webhook,
            locked: webhooksLocked,
          },
        ]
      : []),
    { to: routes.orgAuditLogs(org.id), label: '审计日志', icon: ScrollText, locked: auditLocked },
  ];

  const quotaDisabled = org.link_quota === 0;
  const quotaExceeded = !quotaDisabled && org.link_usage >= org.link_quota;
  const quotaPct = quotaDisabled
    ? 100
    : Math.min(100, Math.round((org.link_usage / org.link_quota) * 100));
  const quotaNearLimit = !quotaDisabled && !quotaExceeded && quotaPct >= 80;

  function handleSwitchOrg(nextOrgId: number) {
    if (nextOrgId === org.id) return;
    setLastOrgId(nextOrgId);
    const section: OrgSection = isMembersRoute
      ? 'members'
      : isAPIKeysRoute
        ? 'api-keys'
        : isWebhooksRoute
          ? 'webhooks'
          : isAuditLogsRoute
            ? 'audit-logs'
            : 'links';
    navigate(routes.orgSection(nextOrgId, section));
    setMobileMenuOpen(false);
  }

  async function handleLogout() {
    try {
      await logout.mutateAsync();
      toast.success('已退出登录');
      navigate(routes.login(), { replace: true });
    } catch (err) {
      toast.error(getErrorMessage(err));
    }
  }

  const userDisplayName = user.name || user.email || `用户 #${user.id}`;

  function renderNavGroup(title: string, items: NavEntry[]) {
    return (
      <div className="space-y-1">
        <div className="px-3 pb-1 text-[11px] font-semibold tracking-wider text-muted-foreground">
          {title}
        </div>
        {items.map((item) => {
          const Icon = item.icon;
          return (
            <NavLink
              key={item.to}
              to={item.to}
              onClick={() => setMobileMenuOpen(false)}
              className={({ isActive }) =>
                cn(
                  'group flex items-center justify-between gap-2 rounded-lg px-3 py-2 text-sm font-medium transition-all focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink',
                  isActive
                    ? 'hl-swipe-yellow font-semibold text-ink'
                    : item.locked
                      ? 'text-muted-foreground hover:bg-paper-warm hover:text-ink'
                      : 'text-ink-secondary hover:bg-paper-warm hover:text-ink',
                )
              }
            >
              {({ isActive }) => (
                <>
                  <span className="flex items-center gap-2.5">
                    <Icon
                      className={cn(
                        'size-4 shrink-0 transition-colors',
                        isActive ? 'text-ink' : 'text-ink-secondary group-hover:text-ink',
                      )}
                    />
                    {item.label}
                  </span>
                  {item.locked && (
                    <Tooltip>
                      <TooltipTrigger asChild>
                        <span
                          aria-label="仅管理员可操作"
                          className="inline-flex size-5 items-center justify-center rounded text-muted-foreground"
                        >
                          <Lock className="size-3.5" />
                        </span>
                      </TooltipTrigger>
                      <TooltipContent side="right">仅所有者与管理员可管理此项</TooltipContent>
                    </Tooltip>
                  )}
                </>
              )}
            </NavLink>
          );
        })}
      </div>
    );
  }

  const sidebarContent = (
    <div className="flex h-full flex-col bg-paper">
      <div className="flex h-16 items-center px-4">
        <BrandLogo />
      </div>

      {/* 组织切换器 */}
      <div className="px-3 pb-4">
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <button
              type="button"
              className="flex w-full items-center gap-2.5 rounded-xl border-[1.25px] border-ink bg-card px-2.5 py-2 text-left ink-shadow-sm transition-all hover:bg-paper-warm/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink cursor-pointer"
            >
              <StampAvatar seed={`org-${org.id}-${org.name}`} label={org.name} size="sm" />
              <div className="min-w-0 flex-1">
                <div className="truncate text-sm font-semibold text-ink">{org.name}</div>
                <div className="text-[11px] font-medium text-muted-foreground">
                  {ROLE_LABELS[org.role]}
                </div>
              </div>
              <ChevronsUpDown className="size-3.5 shrink-0 text-ink-secondary" />
            </button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start" className="w-60">
            <DropdownMenuLabel>切换组织工作区</DropdownMenuLabel>
            {organizations.map((item) => (
              <DropdownMenuItem
                key={item.id}
                onSelect={() => handleSwitchOrg(item.id)}
                className="justify-between gap-2 py-2"
              >
                <span className="flex min-w-0 items-center gap-2">
                  <StampAvatar seed={`org-${item.id}-${item.name}`} label={item.name} size="sm" />
                  <span className="truncate font-medium text-ink">{item.name}</span>
                </span>
                <span className="flex shrink-0 items-center gap-1.5">
                  <span className="text-[11px] text-muted-foreground">
                    {ROLE_LABELS[item.role]}
                  </span>
                  {item.id === org.id && <Check className="size-3.5 text-ink" />}
                </span>
              </DropdownMenuItem>
            ))}
            <DropdownMenuSeparator />
            <DropdownMenuItem onSelect={() => setCreateOrgOpen(true)} className="font-medium">
              <Plus className="size-4" />
              新建组织
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      <nav aria-label="主导航" className="flex-1 space-y-5 overflow-y-auto px-3">
        {renderNavGroup('核心', coreNav)}
        {renderNavGroup('组织与集成', governanceNav)}
      </nav>

      {/* 月度短链配额手绘进度卡 */}
      <div className="px-3 pb-2.5">
        <div className="rounded-xl border-[1.25px] border-ink/85 bg-card p-3">
          <div className="flex items-center justify-between text-xs">
            <span className="font-medium text-ink">本月配额</span>
            <span
              className={cn(
                'font-mono font-semibold tabular-nums',
                quotaDisabled || quotaExceeded
                  ? 'text-stamp-red'
                  : quotaNearLimit
                    ? 'text-amber-800'
                    : 'text-ink',
              )}
            >
              {formatNumber(org.link_usage)} / {formatNumber(org.link_quota)}
            </span>
          </div>
          <div className="mt-2 h-2.5 overflow-hidden rounded-full border border-ink bg-paper-warm">
            <div
              className={cn(
                'h-full transition-all',
                quotaDisabled || quotaExceeded
                  ? 'hachure-red'
                  : quotaNearLimit
                    ? 'hachure-amber'
                    : 'hachure-mint',
              )}
              style={{ width: `${quotaPct}%` }}
            />
          </div>
          <p className="mt-1.5 text-[11px] leading-snug text-muted-foreground">
            {quotaDisabled
              ? '当前组织未开放创建额度，请联系管理员调整'
              : quotaExceeded
                ? '本月配额已用完，将于下月 1 日自动重置'
                : '按每月创建条数计算，删除不返还'}
          </p>
        </div>
      </div>

      {/* 底部当前登录账号 */}
      <div className="p-3 pt-1">
        <div className="flex items-center gap-2.5 rounded-xl border-[1.25px] border-border-soft bg-card/90 px-2.5 py-2">
          <StampAvatar seed={user.email || `user-${user.id}`} label={userDisplayName} size="sm" />
          <div className="min-w-0 flex-1">
            <div className="truncate text-xs font-semibold text-ink">{userDisplayName}</div>
            {user.email && user.name && (
              <div className="truncate text-[11px] text-muted-foreground">{user.email}</div>
            )}
          </div>
          <Tooltip>
            <TooltipTrigger asChild>
              <Button
                variant="ghost"
                size="icon"
                aria-label="退出登录"
                disabled={logout.isPending}
                className="size-7 text-ink-secondary hover:text-ink"
                onClick={() => void handleLogout()}
              >
                <LogOut className="size-3.5" />
              </Button>
            </TooltipTrigger>
            <TooltipContent side="top">退出登录</TooltipContent>
          </Tooltip>
        </div>
      </div>
    </div>
  );

  return (
    <TooltipProvider delayDuration={250}>
      <div className="flex min-h-screen bg-paper">
        <aside className="hidden w-60 shrink-0 border-r-[1.25px] border-ink/80 lg:block">
          <div className="sticky top-0 h-screen">{sidebarContent}</div>
        </aside>

        {/* 移动端导航抽屉：使用 Radix Sheet 完整支持 ESC、焦点锁与防滚动 */}
        <Sheet open={mobileMenuOpen} onOpenChange={setMobileMenuOpen}>
          <SheetContent side="left" className="p-0">
            <SheetTitle className="sr-only">工作区导航</SheetTitle>
            <SheetDescription className="sr-only">切换组织与访问工作台功能模块</SheetDescription>
            {sidebarContent}
          </SheetContent>
        </Sheet>

        <div className="flex min-w-0 flex-1 flex-col">
          <header className="sticky top-0 z-30 grid h-14 grid-cols-[minmax(2.5rem,1fr)_minmax(0,auto)_minmax(2.5rem,1fr)] items-center gap-2 border-b-[1.25px] border-ink/80 bg-paper/95 px-4 backdrop-blur-xs lg:hidden">
            <button
              type="button"
              aria-label="打开菜单"
              className="inline-flex size-8 justify-self-start items-center justify-center rounded-lg border border-ink/40 bg-card text-ink hover:bg-paper-warm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink cursor-pointer"
              onClick={() => setMobileMenuOpen(true)}
            >
              <Menu className="size-4" />
            </button>
            <span className="truncate text-center text-sm font-bold text-ink">{org.name}</span>
            <Badge variant="outline" className="justify-self-end">
              {ROLE_LABELS[org.role]}
            </Badge>
          </header>

          <main className="mx-auto w-full max-w-5xl flex-1 px-4 py-7 sm:px-8 lg:px-10">
            <Outlet key={org.id} />
          </main>
        </div>

        <CreateOrgDialog
          open={createOrgOpen}
          onOpenChange={setCreateOrgOpen}
          onCreated={(created) => {
            navigate(routes.orgLinks(created.id));
          }}
        />
      </div>
    </TooltipProvider>
  );
}
