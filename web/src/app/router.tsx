import { Navigate, Route, Routes } from 'react-router';
import { APIKeysPage } from '@/features/api-keys/APIKeysPage';
import { LoginPage } from '@/features/auth/LoginPage';
import { LinkStatsPage } from '@/features/links/LinkStatsPage';
import { LinksPage } from '@/features/links/LinksPage';
import { MembersPage } from '@/features/members/MembersPage';
import { OnboardingPage } from '@/features/orgs/OnboardingPage';
import { OrgRouteGuard, RootRedirect } from './AuthGuard';

/*
  AppRouter 声明前端全部 URL 路由。
  搭配后端 internal/console/webhandler.go 的 SPA fallback，
  直接刷新 /orgs/1/links 或 /orgs/1/links/AbC123xy 都会回到 index.html 并由这里接管渲染。
*/
export function AppRouter() {
  return (
    <Routes>
      <Route path="/" element={<RootRedirect />} />
      <Route path="/login" element={<LoginPage />} />
      <Route path="/onboarding" element={<OnboardingPage />} />

      <Route path="/orgs/:orgId" element={<OrgRouteGuard />}>
        <Route index element={<Navigate to="links" replace />} />
        <Route path="links" element={<LinksPage />} />
        <Route path="links/:code" element={<LinkStatsPage />} />
        <Route path="members" element={<MembersPage />} />
        <Route path="api-keys" element={<APIKeysPage />} />
      </Route>

      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}
