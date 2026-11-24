import { Navigate, Route, Routes } from 'react-router';
import { APIKeysPage } from '@/features/api-keys/APIKeysPage';
import { LoginPage } from '@/features/auth/LoginPage';
import { LinkStatsPage } from '@/features/links/LinkStatsPage';
import { LinksPage } from '@/features/links/LinksPage';
import { MembersPage } from '@/features/members/MembersPage';
import { OnboardingPage } from '@/features/orgs/OnboardingPage';
import { OrgRouteGuard, RootRedirect } from './AuthGuard';
import { ROUTE_PATTERNS, routes } from './routes';

export function AppRouter() {
  return (
    <Routes>
      <Route path={ROUTE_PATTERNS.root} element={<RootRedirect />} />
      <Route path={ROUTE_PATTERNS.login} element={<LoginPage />} />
      <Route path={ROUTE_PATTERNS.onboarding} element={<OnboardingPage />} />

      <Route path={ROUTE_PATTERNS.orgRoot} element={<OrgRouteGuard />}>
        <Route index element={<Navigate to="links" replace />} />
        <Route path="links" element={<LinksPage />} />
        <Route path="links/:code" element={<LinkStatsPage />} />
        <Route path="members" element={<MembersPage />} />
        <Route path="api-keys" element={<APIKeysPage />} />
      </Route>

      <Route path="*" element={<Navigate to={routes.root()} replace />} />
    </Routes>
  );
}
