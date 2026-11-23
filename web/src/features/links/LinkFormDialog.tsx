import { type FormEvent, useState } from 'react';
import { toast } from 'sonner';
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
import { localDatetimeToISO } from '@/lib/format';
import { type AdminLink, useCreateLink, useUpdateLink } from './api';

interface LinkFormDialogProps {
  orgId: number;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  editingLink?: AdminLink | null;
}

/*
  为什么把表单拆成内部组件并通过 key 挂载：
  React 中推荐用 key 重置表单初始值，而不是在 useEffect 里调用 setState 同步 props。
  这样逻辑更纯粹，也天然避免旧弹窗输入状态泄漏到下一次打开。
*/
export function LinkFormDialog({
  orgId,
  open,
  onOpenChange,
  editingLink,
}: LinkFormDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <LinkFormInner
          key={editingLink ? `edit-${editingLink.code}` : 'create-new'}
          orgId={orgId}
          editingLink={editingLink ?? null}
          onClose={() => onOpenChange(false)}
        />
      </DialogContent>
    </Dialog>
  );
}

interface LinkFormInnerProps {
  orgId: number;
  editingLink: AdminLink | null;
  onClose: () => void;
}

function LinkFormInner({ orgId, editingLink, onClose }: LinkFormInnerProps) {
  const isEdit = editingLink !== null;
  const [url, setUrl] = useState(editingLink?.url ?? '');
  const [expiresAt, setExpiresAt] = useState('');
  const [clearExpires, setClearExpires] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);

  const createLink = useCreateLink(orgId);
  const updateLink = useUpdateLink(orgId);
  const isPending = createLink.isPending || updateLink.isPending;

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const trimmedUrl = url.trim();
    if (!trimmedUrl) {
      setFormError('请输入有效的 http/https 目标地址');
      return;
    }
    setFormError(null);

    try {
      if (isEdit) {
        const payload: { url?: string; expires_at?: string | null } = { url: trimmedUrl };
        if (clearExpires) {
          payload.expires_at = null;
        } else if (expiresAt.trim()) {
          const iso = localDatetimeToISO(expiresAt);
          if (!iso) {
            setFormError('过期时间格式无效');
            return;
          }
          payload.expires_at = iso;
        }
        await updateLink.mutateAsync({ code: editingLink.code, payload });
        toast.success(`已更新短链 /r/${editingLink.code}`);
      } else {
        const iso = localDatetimeToISO(expiresAt);
        const created = await createLink.mutateAsync({
          url: trimmedUrl,
          ...(iso ? { expires_at: iso } : {}),
        });
        toast.success(`已生成短链 /r/${created.code}`);
      }
      onClose();
    } catch (err) {
      setFormError(getErrorMessage(err));
    }
  }

  return (
    <>
      <DialogHeader>
        <DialogTitle>{isEdit ? `编辑短链 /r/${editingLink.code}` : '创建短链'}</DialogTitle>
        <DialogDescription>
          {isEdit
            ? '修改目标 URL 后会自动写穿缓存并触发一次异步风控扫描。'
            : '只支持 http/https 绝对 URL，提交后立刻生成 8 位随机短码。'}
        </DialogDescription>
      </DialogHeader>

      <form className="space-y-4" onSubmit={handleSubmit}>
        <div className="space-y-2">
          <Label htmlFor="link-url">目标 URL</Label>
          <Input
            id="link-url"
            type="url"
            required
            autoFocus
            placeholder="https://example.com/landing?ref=shortlink"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
          />
        </div>

        <div className="space-y-2">
          <Label htmlFor="link-expires">
            {isEdit ? '新过期时间（留空保持不变）' : '过期时间（可选，留空永不过期）'}
          </Label>
          <Input
            id="link-expires"
            type="datetime-local"
            disabled={clearExpires}
            value={expiresAt}
            onChange={(e) => setExpiresAt(e.target.value)}
          />
          {isEdit && editingLink.expires_at && (
            <label className="mt-2 inline-flex items-center gap-2 text-sm text-foreground cursor-pointer">
              <input
                type="checkbox"
                className="size-4 rounded border-input accent-primary"
                checked={clearExpires}
                onChange={(e) => setClearExpires(e.target.checked)}
              />
              清除现有过期时间（改为永不过期）
            </label>
          )}
        </div>

        {formError && (
          <div className="rounded-md border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700">
            {formError}
          </div>
        )}

        <DialogFooter>
          <Button variant="outline" disabled={isPending} onClick={onClose}>
            取消
          </Button>
          <Button type="submit" disabled={isPending}>
            {isPending ? '保存中…' : isEdit ? '保存修改' : '生成短链'}
          </Button>
        </DialogFooter>
      </form>
    </>
  );
}
