const SHORT_CODE_RE = /^[0-9A-Za-z]{8}$/;

/*
  构造可点击、可复制、可生成二维码的完整公开短链地址：
  优先使用后端下发的 PUBLIC_BASE_URL（避免 console 与 redirector 分端口/分域名时点开 404）；
  单入口反向代理部署（未配置 PUBLIC_BASE_URL）时自动回退到当前站点 origin。
*/
export function buildShortUrl(code: string, publicBaseUrl?: string): string {
  const base = (publicBaseUrl?.trim() || window.location.origin).replace(/\/+$/, '');
  return `${base}/r/${code}`;
}

// 短链展示文案：若配置了独立的公开域名，展示 host/r/code 让用户一眼看清最终发出去的域名。
export function formatShortLabel(code: string, publicBaseUrl?: string): string {
  if (!publicBaseUrl?.trim()) {
    return `/r/${code}`;
  }
  try {
    const u = new URL(publicBaseUrl);
    return `${u.host}/r/${code}`;
  } catch {
    return `/r/${code}`;
  }
}

export function isExactShortCode(raw: string): boolean {
  return SHORT_CODE_RE.test(raw.trim());
}
