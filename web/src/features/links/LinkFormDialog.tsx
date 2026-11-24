import { type FormEvent, useState } from 'react';
import { CheckCircle2, ExternalLink } from 'lucide-react';
import { toast } from 'sonner';
import { CopyButton } from '@/components/CopyButton';
import { ErrorAlert } from '@/components/ErrorAlert';
import { QRCodeCard } from '@/components/QRCodeCard';
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
import { usePublicBaseUrl } from '@/features/auth/api';
import { getErrorMessage } from '@/lib/api-client';
import { isoToLocalDatetime, localDatetimeToISO } from '@/lib/format';
import { buildShortUrl } from '@/lib/short-url';
import { type AdminLink, useCreateLink, useUpdateLink } from './api';

interface LinkFormDialogProps {
  orgId: number;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  editingLink?: AdminLink | null;
}

/*
  用 key 挂载内部表单（而不是在 useEffect 里 setState 同步 props），
  保证每次打开弹窗都有干净初始值。新建成功后直接切换到「生成成功态」，
  让用户零跳转完成复制短链与保存二维码。
*/
export function LinkFormDialog({ orgId, open, onOpenChange, editingLink }: LinkFormDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        {open && (
          <LinkFormInner
            key={editingLink ? `edit-${editingLink.code}` : 'create-new'}
            orgId={orgId}
            editingLink={editingLink ?? null}
            onClose={() => onOpenChange(false)}
          />
        )}
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
  const initialExpires = isoToLocalDatetime(editingLink?.expires_at);
  const publicBaseUrl = usePublicBaseUrl();

  const [url, setUrl] = useState(editingLink?.url ?? '');
  const [expiresAt, setExpiresAt] = useState(initialExpires);
  const [clearExpires, setClearExpires] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const [createdLink, setCreatedLink] = useState<AdminLink | null>(null);

  const createLink = useCreateLink(orgId);
  const updateLink = useUpdateLink(orgId);
  const isPending = createLink.isPending || updateLink.isPending;

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const trimmedUrl = url.trim();
    if (!trimmedUrl) {
      setFormError('请输入有效的 http 或 https 目标地址');
      return;
    }
    setFormError(null);

    try {
      if (isEdit) {
        const payload: { url?: string; expires_at?: string | null } = { url: trimmedUrl };
        if (clearExpires) {
          payload.expires_at = null;
        } else if (expiresAt.trim() && expiresAt !== initialExpires) {
          // 仅在用户主动改动过过期时间时提交，避免编辑已过期链接的 URL 时把旧的过去时间重新发回后端。
          const iso = localDatetimeToISO(expiresAt);
          if (!iso) {
            setFormError('过期时间格式无效');
            return;
          }
          payload.expires_at = iso;
        }
        await updateLink.mutateAsync({ code: editingLink.code, payload });
        toast.success(`已更新短链 /r/${editingLink.code}`);
        onClose();
      } else {
        const iso = localDatetimeToISO(expiresAt);
        const created = await createLink.mutateAsync({
          url: trimmedUrl,
          ...(iso ? { expires_at: iso } : {}),
        });
        toast.success(`已生成短链 /r/${created.code}`);
        setCreatedLink(created);
      }
    } catch (err) {
      setFormError(getErrorMessage(err));
    }
  }

  function handleCreateAnother() {
    setCreatedLink(null);
    setUrl('');
    setExpiresAt('');
    setClearExpires(false);
    setFormError(null);
  }

  if (createdLink) {
    const shortUrl = buildShortUrl(createdLink.code, publicBaseUrl);
    return (
      <>
        <DialogHeader>
          <div className="flex items-center gap-2 text-emerald-700">
            <CheckCircle2 className="size-5" />
            <DialogTitle>短链已生成</DialogTitle>
          </div>
          <DialogDescription>
            短链已生效，你可以立即复制链接或保存二维码用于分享。
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          <div className="flex items-center gap-2 rounded-lg border border-zinc-200 bg-zinc-50 px-3 py-2.5">
            <a
              href={shortUrl}
              target="_blank"
              rel="noreferrer"
              data-testid="created-short-url"
              className="min-w-0 flex-1 truncate font-mono text-sm font-semibold text-brand hover:underline"
            >
              {shortUrl}
            </a>
            <ExternalLink className="size-3.5 shrink-0 text-zinc-400" />
            <CopyButton
              value={shortUrl}
              variant="pill"
              size="sm"
              label="复制短链"
              toastMessage={`已复制 ${shortUrl}`}
            />
          </div>

          <QRCodeCard url={shortUrl} code={createdLink.code} />
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={handleCreateAnother}>
            继续创建下一条
          </Button>
          <Button onClick={onClose}>完成</Button>
        </DialogFooter>
      </>
    );
  }

  return (
    <>
      <DialogHeader>
        <DialogTitle>{isEdit ? `编辑短链 /r/${editingLink.code}` : '创建短链'}</DialogTitle>
        <DialogDescription>
          {isEdit
            ? '保存后新地址立即生效，系统会在后台自动复检目标链接安全性。'
            : '支持 http / https 网址，提交后即刻生成随机短码与分享二维码。'}
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
            {isEdit ? '过期时间（可调整，留空保持不变）' : '过期时间（可选，留空永不过期）'}
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

        {formError && <ErrorAlert compact message={formError} />}

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
