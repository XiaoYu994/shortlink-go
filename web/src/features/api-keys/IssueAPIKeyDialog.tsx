import { type FormEvent, useState } from 'react';
import { toast } from 'sonner';
import { ErrorAlert } from '@/components/ErrorAlert';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import { Input, Label } from '@/components/ui/input';
import { getErrorMessage } from '@/lib/api-client';
import { MAX_API_KEY_NAME_BYTES } from '@/lib/constants';
import { localDatetimeToISO } from '@/lib/format';
import { ALL_API_KEY_SCOPES, type APIKeyScope, type IssuedAPIKey, useIssueAPIKey } from './api';

const utf8Encoder = new TextEncoder();

interface IssueAPIKeyDialogProps {
  orgId: number;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onIssued: (issued: IssuedAPIKey) => void;
}

export function IssueAPIKeyDialog({ orgId, open, onOpenChange, onIssued }: IssueAPIKeyDialogProps) {
  const [name, setName] = useState('');
  const [scopes, setScopes] = useState<APIKeyScope[]>(['links:read', 'links:write', 'stats:read']);
  const [expiresAt, setExpiresAt] = useState('');
  const [formError, setFormError] = useState<string | null>(null);
  const issueKey = useIssueAPIKey(orgId);

  function handleOpenChange(nextOpen: boolean) {
    if (!nextOpen) {
      setName('');
      setScopes(['links:read', 'links:write', 'stats:read']);
      setExpiresAt('');
      setFormError(null);
    }
    onOpenChange(nextOpen);
  }

  function toggleScope(scope: APIKeyScope) {
    setScopes((prev) =>
      prev.includes(scope) ? prev.filter((item) => item !== scope) : [...prev, scope],
    );
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const trimmedName = name.trim();
    if (utf8Encoder.encode(trimmedName).length > MAX_API_KEY_NAME_BYTES) {
      setFormError(`名称不能超过 ${MAX_API_KEY_NAME_BYTES} 字节`);
      return;
    }
    if (scopes.length === 0) {
      setFormError('请至少选择一个权限 scope');
      return;
    }
    setFormError(null);
    try {
      const iso = localDatetimeToISO(expiresAt);
      const issued = await issueKey.mutateAsync({
        ...(trimmedName ? { name: trimmedName } : {}),
        scopes,
        ...(iso ? { expires_at: iso } : {}),
      });
      toast.success(
        issued.name ? `已签发 API Key「${issued.name}」` : `已签发 API Key #${issued.id}`,
      );
      handleOpenChange(false);
      onIssued(issued);
    } catch (err) {
      setFormError(getErrorMessage(err));
    }
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>签发组织 API Key</DialogTitle>
          <DialogDescription>
            API Key 仅用于服务端/脚本调用短链与统计接口，不能管理组织成员或其他 API Key。
          </DialogDescription>
        </DialogHeader>
        <form className="space-y-4" onSubmit={handleSubmit}>
          <div className="space-y-2">
            <Label htmlFor="apikey-name">名称 / 备注（可选）</Label>
            <Input
              id="apikey-name"
              placeholder="例如：生产 CI 发布、数据分析脚本"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </div>

          <div className="space-y-2">
            <Label>权限范围（Scopes）</Label>
            <div className="space-y-2 rounded-lg border border-border bg-slate-50/70 p-3">
              {ALL_API_KEY_SCOPES.map((item) => (
                <label key={item.value} className="flex items-start gap-2.5 text-sm cursor-pointer">
                  <input
                    type="checkbox"
                    className="mt-0.5 size-4 rounded border-input accent-primary"
                    checked={scopes.includes(item.value)}
                    onChange={() => toggleScope(item.value)}
                  />
                  <span>
                    <span className="font-mono text-xs font-semibold text-foreground">
                      {item.title}
                    </span>
                    <span className="block text-xs text-muted-foreground">{item.description}</span>
                  </span>
                </label>
              ))}
            </div>
          </div>

          <div className="space-y-2">
            <Label htmlFor="apikey-expires">过期时间（可选，留空永不过期）</Label>
            <Input
              id="apikey-expires"
              type="datetime-local"
              value={expiresAt}
              onChange={(e) => setExpiresAt(e.target.value)}
            />
          </div>

          {formError && <ErrorAlert compact message={formError} />}

          <DialogFooter>
            <Button
              variant="outline"
              disabled={issueKey.isPending}
              onClick={() => handleOpenChange(false)}
            >
              取消
            </Button>
            <Button type="submit" disabled={issueKey.isPending}>
              {issueKey.isPending ? '签发中…' : '确认签发'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
