import { useEffect } from 'react';

const APP_TITLE = 'Ori · 折羽';

// 动态更新浏览器标签页标题，切换页面与组织时保持清晰定位。
export function useDocumentTitle(pageTitle?: string): void {
  useEffect(() => {
    document.title = pageTitle ? `${pageTitle} · ${APP_TITLE}` : APP_TITLE;
    return () => {
      document.title = APP_TITLE;
    };
  }, [pageTitle]);
}
