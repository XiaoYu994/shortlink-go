const BAN_REASON_LABELS: Record<string, string> = {
  phishing: '钓鱼风险',
  malware: '恶意软件',
  adult: '违规色情',
  gambling: '涉赌风险',
  drugs: '违禁药品',
  spam: '垃圾推广',
  other: '违规内容',
};

const DEVICE_LABELS: Record<string, string> = {
  desktop: '桌面端',
  mobile: '移动端',
  tablet: '平板',
  bot: '机器人 / 爬虫',
  other: '其他设备',
};

const CLIENT_LABELS: Record<string, string> = {
  browser: '浏览器',
  bot: '机器人 / 爬虫',
  cli: '命令行工具',
  other: '其他客户端',
};

export function formatBanReason(reason?: string): string {
  if (!reason) return '';
  const key = reason.trim().toLowerCase();
  return BAN_REASON_LABELS[key] ?? reason;
}

export function formatReferrerLabel(referrer: string): string {
  if (referrer === 'direct') return '直接访问 (direct)';
  return referrer;
}

export function formatDeviceLabel(device: string): string {
  return DEVICE_LABELS[device] ?? device;
}

export function formatClientLabel(client: string): string {
  return CLIENT_LABELS[client] ?? client;
}
