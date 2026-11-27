import { type FormEvent, useState } from 'react';
import { toast } from 'sonner';
import { ErrorAlert } from '@/components/ErrorAlert';
import {
  createInitialExpiration,
  type ExpirationSelection,
  resolveExpiration,
} from '@/components/expiration-helpers';
import { ExpirationPicker } from '@/components/ExpirationPicker';
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
import { ALL_API_KEY_SCOPES, type APIKeyScope, type IssuedAPIKey, useIssueAPIKey } from './api';

const utf8Encoder = new TextEncoder();

const SCOPE_TEMPLATES: ReadonlyArray<{ label: string; scopes: APIKeyScope[] }> = [
  { label: '只读（推荐）', scopes: ['links:read'] },
  { label: '短链读写', scopes: ['links:read', 'links:write'] },
  { label: '全量含统计', scopes: ['links:read', 'links:write', 'stats:read'] },
];

interface IssueAPIKeyDialogProps {
  orgId: number;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onIssued: (issued: IssuedAPIKey) => void;
}

export function IssueAPIKeyDialog({ orgId, open, onOpenChange, onIssued }: IssueAPIKeyDialogProps) {
  const [name, setName] = useState('');
  const [scopes, setScopes] = useState<APIKeyScope[]>(['links:read']);
  const [expiration, setExpiration] = useState<ExpirationSelection>(() =>
    createInitialExpiration('create'),
  );
  const [formError, setFormError] = useState<string | null>(null);
  const issueKey = useIssueAPIKey(orgId);

  function handleOpenChange(nextOpen: boolean) {
    if (!nextOpen) {
      setName('');
      setScopes(['links:read']);
      setExpiration(createInitialExpiration('create'));
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
      setFormError('名称过长，请精简后再试');
      return;
    }
    if (scopes.length === 0) {
      setFormError('请至少选择一项访问权限');
      return;
    }
    const exp = resolveExpiration(expiration);
    if (exp.action === 'error') {
      setFormError(exp.message);
      return;
    }
    setFormError(null);
    try {
      const issued = await issueKey.mutateAsync({
        ...(trimmedName ? { name: trimmedName } : {}),
        scopes,
        ...(exp.action === 'set' ? { expires_at: exp.iso } : {}),
      });
      toast.success(
        issued.name ? `已创建 API Key「${issued.name}」` : `已创建 API Key #${issued.id}`,
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
          <DialogTitle>创建组织 API{'\u00A0'}Key</DialogTitle>
          <DialogDescription>仅用于调用短链与统计接口，无成员或密钥管理权限。</DialogDescription>
        </DialogHeader>
        <form className="space-y-4" onSubmit={handleSubmit}>
          <div className="space-y-2">
            <Label htmlFor="apikey-name">名称 / 备注（可选）</Label>
            <Input
              id="apikey-name"
              placeholder="例如：生产构建部署、数据分析脚本"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </div>

          <div className="space-y-2">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <span className="text-sm font-medium leading-none text-ink">访问权限</span>
              <div className="flex flex-wrap gap-1">
                {SCOPE_TEMPLATES.map((tpl) => {
                  const isActive =
                    tpl.scopes.length === scopes.length &&
                    tpl.scopes.every((s) => scopes.includes(s));
                  return (
                    <button
                      key={tpl.label}
                      type="button"
                      aria-pressed={isActive}
                      onClick={() => setScopes(tpl.scopes)}
                      className={
                        isActive
                          ? 'cursor-pointer rounded-md border border-ink bg-hl-yellow px-2 py-0.5 text-[11px] font-semibold text-ink'
                          : 'cursor-pointer rounded-md border border-ink/40 bg-paper-warm px-2 py-0.5 text-[11px] font-medium text-ink-secondary hover:border-ink hover:text-ink'
                      }
                    >
                      {tpl.label}
                    </button>
                  );
                })}
              </div>
            </div>
            <div className="space-y-2 rounded-xl border-[1.25px] border-ink/60 bg-paper-warm/50 p-3">
              {ALL_API_KEY_SCOPES.map((item) => (
                <label key={item.value} className="flex items-start gap-2.5 text-sm cursor-pointer">
                  <input
                    type="checkbox"
                    className="mt-0.5 size-4 rounded border-ink accent-ink"
                    checked={scopes.includes(item.value)}
                    onChange={() => toggleScope(item.value)}
                  />
                  <span>
                    <span className="font-mono text-xs font-bold text-ink">{item.title}</span>
                    <span className="block text-xs text-ink-secondary">{item.description}</span>
                  </span>
                </label>
              ))}
            </div>
          </div>

          <ExpirationPicker
            id="apikey-expires"
            label="密钥有效期（可选）"
            mode="create"
            value={expiration}
            onChange={setExpiration}
          />

          {formError && <ErrorAlert compact message={formError} />}

          <DialogFooter>
            <Button
              variant="outline"
              disabled={issueKey.isPending}
              onClick={() => handleOpenChange(false)}
            >
              取消
            </Button>
            <Button type="submit" variant="highlight" disabled={issueKey.isPending}>
              {issueKey.isPending ? '创建中…' : '确认创建'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
