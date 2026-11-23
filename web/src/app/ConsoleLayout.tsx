import { useState } from 'react';
import {
  Check,
  ChevronsUpDown,
  KeyRound,
  Link2,
  LogOut,
  Menu,
  Plus,
  Users,
  X,
} from 'lucide-react';
import { NavLink, Outlet, useLocation, useNavigate } from 'react-router';
import { toast } from 'sonner';
import { MonogramAvatar } from '@/components/MonogramAvatar';
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
import { useLogout } from '@/features/auth/api';
import { ROLE_LABELS } from '@/features/auth/permissions';
import { setLastOrgId } from '@/features/orgs/api';
import { CreateOrgDialog } from '@/features/orgs/CreateOrgDialog';
import { cn } from '@/lib/cn';
import { useCurrentOrg } from './org-context';

/*
  Linear / Dub.co 风格的 Inset Sidebar Shell（内嵌工作台壳层）：
  左侧 248px 极浅灰导轨承载组织切换与导航，右侧纯白圆角主画布自带呼吸感与细微边界。
*/
export function ConsoleLayout() {
  const { user, org, organizations } = useCurrentOrg();
  const navigate = useNavigate();
  const location = useLocation();
  const logout = useLogout();
  const [createOrgOpen, setCreateOrgOpen] = useState(false);
  const [mobileMenuOpen, setMobileMenuOpen] = useState(false);

  const navItems = [
    { to: `/orgs/${org.id}/links`, label: '短链管理', icon: Link2 },
    { to: `/orgs/${org.id}/members`, label: '成员与邀请', icon: Users },
    { to: `/orgs/${org.id}/api-keys`, label: 'API Keys', icon: KeyRound },
  ];

  function handleSwitchOrg(nextOrgId: number) {
    if (nextOrgId === org.id) return;
    setLastOrgId(nextOrgId);
    const section = location.pathname.split('/')[3];
    const targetSection = section === 'members' || section === 'api-keys' ? section : 'links';
    navigate(`/orgs/${nextOrgId}/${targetSection}`);
    setMobileMenuOpen(false);
  }

  async function handleLogout() {
    try {
      await logout.mutateAsync();
      toast.success('已退出登录');
    } finally {
      navigate('/login', { replace: true });
    }
  }

  const userDisplayName = user.name || user.email || `用户 #${user.id}`;

  const sidebarContent = (
    <div className="flex h-full flex-col">
      <div className="flex h-14 items-center justify-between px-4">
        <div className="flex items-center gap-2.5">
          <div className="flex size-7 items-center justify-center rounded-lg bg-zinc-900 text-white shadow-2xs">
            <Link2 className="size-3.5" />
          </div>
          <span className="text-[15px] font-semibold tracking-tight text-zinc-950">Shortlink</span>
          <span className="rounded-md bg-zinc-200/70 px-1.5 py-0.5 font-mono text-[10px] font-medium text-zinc-600">
            v1
          </span>
        </div>
        <button
          type="button"
          aria-label="关闭菜单"
          className="rounded-md p-1 text-zinc-500 hover:bg-zinc-200/60 lg:hidden"
          onClick={() => setMobileMenuOpen(false)}
        >
          <X className="size-4" />
        </button>
      </div>

      {/* 组织切换器（下拉菜单） */}
      <div className="px-3 pb-3">
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <button
              type="button"
              className="flex w-full items-center gap-2.5 rounded-xl bg-white px-2.5 py-2 text-left shadow-[0_1px_2px_rgba(0,0,0,0.04)] ring-1 ring-zinc-200/90 transition-all hover:ring-zinc-300 cursor-pointer"
            >
              <MonogramAvatar seed={`org-${org.id}-${org.name}`} label={org.name} size="sm" />
              <div className="min-w-0 flex-1">
                <div className="truncate text-sm font-medium text-zinc-950">{org.name}</div>
                <div className="text-[11px] text-zinc-500">
                  {ROLE_LABELS[org.role]} · #{org.id}
                </div>
              </div>
              <ChevronsUpDown className="size-3.5 shrink-0 text-zinc-400" />
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
                  <MonogramAvatar seed={`org-${item.id}-${item.name}`} label={item.name} size="sm" className="size-6 text-[11px]" />
                  <span className="truncate font-medium">{item.name}</span>
                </span>
                <span className="flex shrink-0 items-center gap-1.5">
                  <span className="text-[11px] text-zinc-500">{ROLE_LABELS[item.role]}</span>
                  {item.id === org.id && <Check className="size-3.5 text-brand" />}
                </span>
              </DropdownMenuItem>
            ))}
            <DropdownMenuSeparator />
            <DropdownMenuItem onSelect={() => setCreateOrgOpen(true)} className="text-zinc-700">
              <Plus className="size-4" />
              新建组织
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      <div className="px-4 pb-1.5 pt-2 text-[11px] font-medium tracking-wider text-zinc-400 uppercase">
        工作区
      </div>

      <nav aria-label="主导航" className="flex-1 space-y-0.5 px-3">
        {navItems.map((item) => {
          const Icon = item.icon;
          return (
            <NavLink
              key={item.to}
              to={item.to}
              onClick={() => setMobileMenuOpen(false)}
              className={({ isActive }) =>
                cn(
                  'group flex items-center gap-2.5 rounded-lg px-3 py-2 text-sm font-medium transition-all',
                  isActive
                    ? 'bg-white text-zinc-950 shadow-[0_1px_2px_rgba(0,0,0,0.05)] ring-1 ring-zinc-200/80'
                    : 'text-zinc-600 hover:bg-zinc-200/50 hover:text-zinc-950',
                )
              }
            >
              {({ isActive }) => (
                <>
                  <Icon
                    className={cn(
                      'size-4 shrink-0 transition-colors',
                      isActive ? 'text-brand' : 'text-zinc-400 group-hover:text-zinc-700',
                    )}
                  />
                  {item.label}
                </>
              )}
            </NavLink>
          );
        })}
      </nav>

      {/* 底部当前登录账号 */}
      <div className="p-3">
        <div className="flex items-center gap-2.5 rounded-xl border border-zinc-200/70 bg-white/70 px-2.5 py-2">
          <MonogramAvatar
            seed={user.email || `user-${user.id}`}
            label={userDisplayName}
            size="sm"
          />
          <div className="min-w-0 flex-1">
            <div className="truncate text-xs font-medium text-zinc-900">{userDisplayName}</div>
            {user.email && user.name && (
              <div className="truncate text-[11px] text-zinc-500">{user.email}</div>
            )}
          </div>
          <Button
            variant="ghost"
            size="icon"
            title="退出登录"
            aria-label="退出登录"
            disabled={logout.isPending}
            className="size-7 text-zinc-400 hover:text-zinc-700"
            onClick={() => void handleLogout()}
          >
            <LogOut className="size-3.5" />
          </Button>
        </div>
      </div>
    </div>
  );

  return (
    <div className="flex min-h-screen bg-background">
      {/* 桌面端极简左侧导轨 */}
      <aside className="hidden w-64 shrink-0 lg:block">
        <div className="sticky top-0 h-screen">{sidebarContent}</div>
      </aside>

      {/* 移动端侧边抽屉 */}
      {mobileMenuOpen && (
        <div className="fixed inset-0 z-40 lg:hidden">
          <div
            className="fixed inset-0 bg-zinc-950/35 backdrop-blur-[1px]"
            onClick={() => setMobileMenuOpen(false)}
          />
          <aside className="fixed inset-y-0 left-0 z-50 w-64 border-r border-zinc-200 bg-background shadow-xl">
            {sidebarContent}
          </aside>
        </div>
      )}

      {/* 右侧内嵌纯白主工作区 */}
      <div className="flex min-w-0 flex-1 flex-col lg:py-2.5 lg:pr-2.5">
        <header className="flex h-14 items-center justify-between border-b border-zinc-200/80 bg-white px-4 lg:hidden">
          <button
            type="button"
            aria-label="打开菜单"
            className="rounded-md p-1.5 text-zinc-600 hover:bg-zinc-100"
            onClick={() => setMobileMenuOpen(true)}
          >
            <Menu className="size-5" />
          </button>
          <span className="truncate text-sm font-semibold text-zinc-950">{org.name}</span>
          <Badge variant="outline">{ROLE_LABELS[org.role]}</Badge>
        </header>

        <div className="flex-1 bg-white lg:rounded-2xl lg:shadow-[0_1px_3px_rgba(0,0,0,0.03)] lg:ring-1 lg:ring-zinc-200/80">
          <main className="mx-auto w-full max-w-5xl px-4 py-7 sm:px-8 lg:px-10">
            <Outlet />
          </main>
        </div>
      </div>

      <CreateOrgDialog
        open={createOrgOpen}
        onOpenChange={setCreateOrgOpen}
        onCreated={(created) => {
          navigate(`/orgs/${created.id}/links`);
        }}
      />
    </div>
  );
}
