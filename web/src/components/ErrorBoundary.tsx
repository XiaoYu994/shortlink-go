import { Component, type ErrorInfo, type ReactNode } from 'react';
import { AlertTriangle } from 'lucide-react';
import { Button } from './ui/button';

interface ErrorBoundaryProps {
  children: ReactNode;
}

interface ErrorBoundaryState {
  error: Error | null;
}

/*
  ErrorBoundary 相当于 Go HTTP 里的 recoverer 中间件：
  捕获子树渲染期抛出的未预期异常，防止整个页面直接白屏崩溃。
  注意：React 目前仍要求用 Class 组件实现 getDerivedStateFromError。
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
      return (
        <div className="flex min-h-screen items-center justify-center bg-background p-6">
          <div className="w-full max-w-md rounded-xl border border-border bg-card p-6 text-center shadow-sm">
            <div className="mx-auto mb-3 flex size-11 items-center justify-center rounded-full bg-red-50 text-destructive">
              <AlertTriangle className="size-5" />
            </div>
            <h1 className="text-lg font-semibold">页面出现意外错误</h1>
            <p className="mt-1.5 text-sm text-muted-foreground">
              {this.state.error.message || '渲染组件时发生未捕获异常。'}
            </p>
            <div className="mt-5 flex justify-center gap-2">
              <Button variant="outline" onClick={() => this.setState({ error: null })}>
                重试渲染
              </Button>
              <Button onClick={() => window.location.reload()}>刷新页面</Button>
            </div>
          </div>
        </div>
      );
    }

    return this.props.children;
  }
}
