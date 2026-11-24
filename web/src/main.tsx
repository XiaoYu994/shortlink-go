import { StrictMode } from 'react';
import { QueryClientProvider } from '@tanstack/react-query';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router';
import { Toaster } from 'sonner';
import { AppRouter } from '@/app/router';
import { ErrorBoundary } from '@/components/ErrorBoundary';
import { authKeys } from '@/features/auth/api';
import { createQueryClient } from '@/lib/query-client';
import './index.css';

const queryClient = createQueryClient({
  onOrgAccessDenied: () => {
    void queryClient.invalidateQueries({ queryKey: authKeys.me });
  },
});

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ErrorBoundary>
      <QueryClientProvider client={queryClient}>
        <BrowserRouter>
          <AppRouter />
        </BrowserRouter>
        <Toaster position="top-right" richColors closeButton />
      </QueryClientProvider>
    </ErrorBoundary>
  </StrictMode>,
);
