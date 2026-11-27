import { Component, type ErrorInfo, type ReactNode } from 'react';
import { AlertTriangle } from 'lucide-react';
import { CopyButton } from './CopyButton';
import { Button } from './ui/button';

interface ErrorBoundaryProps {
  children: ReactNode;
}

interface ErrorBoundaryState {
  error: Error | null;
}

/*
  ErrorBoundary 相当于 Go HTTP 里的 recoverer 中间件：
  捕获子树渲染期抛出的未预期异常，向用户展示温和中文引导并折叠原始技术详情。
*/
export class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
  state: ErrorBoundaryState = { error: null };

  static getDerivedStateFromError(error: Error): ErrorBoundaryState {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('Uncaught UI error:', error, info.componentStack);
  }

  render() {
    if (this.state.error) {
      const detail = this.state.error.message || '渲染组件时发生未捕获异常。';
      return (
        <div className="flex min-h-screen items-center justify-center bg-paper-dots p-6">
          <div className="w-full max-w-md rounded-2xl border-[1.5px] border-ink bg-card p-6 text-center ink-shadow">
            <div className="mx-auto mb-3 flex size-12 items-center justify-center rounded-xl border-[1.25px] border-ink bg-red-50 text-stamp-red">
              <AlertTriangle className="size-5" />
            </div>
            <h1 className="text-lg font-bold text-ink">页面加载遇到了点小波折</h1>
            <p className="mt-1.5 text-sm leading-relaxed text-ink-secondary">
              界面渲染时出现意外中断，你可以重试渲染或刷新页面恢复工作。
            </p>
            <details className="mt-4 rounded-xl border border-border-soft bg-paper-warm/70 p-3 text-left">
              <summary className="cursor-pointer text-xs font-medium text-ink-secondary select-none">
                查看技术详情
              </summary>
              <div className="mt-2 flex items-start justify-between gap-2">
                <code className="break-all font-mono text-[11px] text-ink-secondary">{detail}</code>
                <CopyButton value={detail} label="复制错误详情" toastMessage="已复制错误详情" />
              </div>
            </details>
            <div className="mt-5 flex justify-center gap-2">
              <Button variant="outline" onClick={() => this.setState({ error: null })}>
                重试渲染
              </Button>
              <Button variant="highlight" onClick={() => window.location.reload()}>
                刷新页面
              </Button>
            </div>
          </div>
        </div>
      );
    }

    return this.props.children;
  }
}
