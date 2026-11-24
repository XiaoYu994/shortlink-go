/*
  将后端英文哨兵错误统一映射为面向终端用户的中文提示（前后端契约仍保持英文稳定值，方便 API 客户端程序化判断）。
*/
const EXACT_MESSAGES: Record<string, string> = {
  unauthorized: '登录状态已失效，请重新登录',
  forbidden: '你当前的角色没有执行此操作的权限',
  'not found': '目标资源不存在',
  'link not found': '短链不存在或已被删除',
  'organization not found': '组织不存在，或你已不再是该组织成员',
  'member not found': '该成员已不在当前组织中',
  'invitation not found': '该邀请记录不存在',
  'api key not found': '该 API Key 不存在',
  'link is banned': '该短链因触发安全风控已被平台封禁，无法修改或删除',
  'link quota exceeded': '本月短链创建配额已用完，将于下月 1 日（UTC）自动重置',
  'rate limit exceeded': '操作过于频繁，请稍候片刻再试',
  'organization limit exceeded': '你作为所有者创建的组织数量已达上限（最多 10 个）',
  'organization must have at least one owner':
    '为避免组织无人管理，至少需要保留一位所有者（Owner）',
  'pending invitation already exists': '该邮箱已有待兑现的邀请，请先撤销旧邀请再重新发起',
  'invitation is no longer pending': '该邀请已被兑现、撤销或已过期，无法再次撤销',
  'api key already revoked': '该 API Key 此前已被吊销',
  'invalid url': '请输入有效的 http 或 https 链接地址',
  'invalid email': '请输入格式正确的邮箱地址',
  'invalid role': '所选组织角色无效',
  'invalid organization name': '组织名称不能为空且不能超过 100 字节',
  'invalid api key scope': '请至少选择一个有效的权限范围',
  'invalid action': '所选审计动作筛选条件无效',
  'invalid cursor': '分页游标无效，请刷新页面后重试',
  'invalid expires_at': '过期时间格式无效，请重新选择',
  'expires_at must be in the future': '过期时间必须晚于当前时间',
  'cross-origin request rejected': '请求来源校验未通过，请刷新页面后重试',
  'request timed out': '请求超时，请检查网络后重试',
  'internal error': '服务暂时不可用，请稍后重试',
};

export function translateAPIError(raw: string, status?: number): string {
  const key = raw.trim().toLowerCase();
  if (Object.hasOwn(EXACT_MESSAGES, key)) {
    const exact = EXACT_MESSAGES[key];
    if (exact !== undefined) return exact;
  }
  if (key.includes('expires_at') && key.includes('future')) {
    return '过期时间必须晚于当前时间';
  }
  if (key.includes('invalid url') || key.includes('url is required')) {
    return '请输入有效的 http 或 https 链接地址';
  }
  if (status === 401) return '登录状态已失效，请重新登录';
  if (status === 403) return '你当前的角色没有执行此操作的权限';
  if (status === 429) return '操作过于频繁，请稍候片刻再试';
  if (status !== undefined && status >= 500) return '服务暂时不可用，请稍后重试';
  return raw || '请求失败，请稍后重试';
}
