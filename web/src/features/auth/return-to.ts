/*
  sanitizeReturnTo 校验并规范化登录成功后的站内回跳路径，与后端 authhandler 保持同构：
  拒绝协议相对路径（//）、反斜杠、控制字符、超长路径，并在清理 .. 段后禁止落入 /api 或 /r。
*/
export function sanitizeReturnTo(raw: string | null): string {
  if (!raw || raw.length > 1024) return '/';
  if (!raw.startsWith('/') || raw.startsWith('//') || raw.includes('\\')) return '/';
  for (let i = 0; i < raw.length; i++) {
    const code = raw.charCodeAt(i);
    if (code < 0x20 || code === 0x7f) return '/';
  }
  const qIdx = raw.indexOf('?');
  const pathPart = qIdx >= 0 ? raw.slice(0, qIdx) : raw;
  const queryPart = qIdx >= 0 ? raw.slice(qIdx) : '';
  const segments: string[] = [];
  for (const seg of pathPart.split('/')) {
    if (seg === '' || seg === '.') continue;
    if (seg === '..') {
      segments.pop();
      continue;
    }
    segments.push(seg);
  }
  const cleanPath = '/' + segments.join('/');
  if (
    cleanPath === '/api' ||
    cleanPath.startsWith('/api/') ||
    cleanPath === '/r' ||
    cleanPath.startsWith('/r/')
  ) {
    return '/';
  }
  return cleanPath + queryPart;
}
