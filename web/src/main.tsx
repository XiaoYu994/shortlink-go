import '@fontsource-variable/plus-jakarta-sans';
import '@fontsource-variable/jetbrains-mono';
import { StrictMode } from 'react';
import { QueryClientProvider } from '@tanstack/react-query';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router';
import { Toaster } from 'sonner';
import { AppRouter } from '@/app/router';
import { ErrorBoundary } from '@/components/ErrorBoundary';
import { TooltipProvider } from '@/components/ui/tooltip';
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
        <TooltipProvider delayDuration={250}>
          <BrowserRouter>
            <AppRouter />
          </BrowserRouter>
          <Toaster
            position="bottom-right"
            closeButton
            toastOptions={{
              classNames: {
                toast:
                  'border-[1.5px] border-ink bg-card text-ink shadow-[3px_3px_0_#1F1E1B] rounded-xl font-sans',
                title: 'text-sm font-medium text-ink',
                description: 'text-xs text-ink-secondary',
                closeButton: 'border border-ink/40 bg-paper text-ink hover:bg-hl-yellow',
                success: '!bg-[#F2FCF6] !border-ink',
                error: '!bg-[#FEF2F2] !border-stamp-red !text-[#991B1B]',
                info: '!bg-[#FEFCE8] !border-ink',
              },
            }}
          />
        </TooltipProvider>
      </QueryClientProvider>
    </ErrorBoundary>
  </StrictMode>,
);
