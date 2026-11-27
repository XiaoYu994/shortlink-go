import { type FormEvent, useState } from 'react';
import { ExternalLink, Sparkles } from 'lucide-react';
import { toast } from 'sonner';
import { CopyButton } from '@/components/CopyButton';
import { CourierCat } from '@/components/CourierCat';
import { ErrorAlert } from '@/components/ErrorAlert';
import {
  createInitialExpiration,
  type ExpirationSelection,
  resolveExpiration,
} from '@/components/expiration-helpers';
import { ExpirationPicker } from '@/components/ExpirationPicker';
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
import { buildShortUrl } from '@/lib/short-url';
import { type AdminLink, useCreateLink, useUpdateLink } from './api';

interface LinkFormDialogProps {
  orgId: number;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  editingLink?: AdminLink | null;
}

function needsHttpsPrefix(raw: string): boolean {
  const trimmed = raw.trim();
  if (!trimmed || /^https?:\/\//i.test(trimmed)) return false;
  return /^[^\s/]+\.[^\s]+/.test(trimmed);
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
  const publicBaseUrl = usePublicBaseUrl();

  const [url, setUrl] = useState(editingLink?.url ?? '');
  const [expiration, setExpiration] = useState<ExpirationSelection>(() =>
    createInitialExpiration(isEdit ? 'edit' : 'create', editingLink?.expires_at),
  );
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

    const expResult = resolveExpiration(expiration, editingLink?.expires_at);
    if (expResult.action === 'error') {
      setFormError(expResult.message);
      return;
    }
    setFormError(null);

    try {
      if (isEdit) {
        const payload: { url?: string; expires_at?: string | null } = { url: trimmedUrl };
        if (expResult.action === 'clear') {
          payload.expires_at = null;
        } else if (expResult.action === 'set') {
          payload.expires_at = expResult.iso;
        }
        await updateLink.mutateAsync({ code: editingLink.code, payload });
        toast.success(`已更新短链 /r/${editingLink.code}`);
        onClose();
      } else {
        const created = await createLink.mutateAsync({
          url: trimmedUrl,
          ...(expResult.action === 'set' ? { expires_at: expResult.iso } : {}),
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
    setExpiration(createInitialExpiration('create'));
    setFormError(null);
  }

  if (createdLink) {
    const shortUrl = buildShortUrl(createdLink.code, publicBaseUrl);
    return (
      <>
        <DialogHeader>
          <div className="flex items-center gap-3">
            <CourierCat pose="success" className="size-16 shrink-0" />
            <div className="space-y-1">
              <DialogTitle>短链已生成</DialogTitle>
              <DialogDescription>复制短链或下载二维码即可分享。</DialogDescription>
            </div>
          </div>
        </DialogHeader>

        <div className="space-y-4">
          <div className="flex items-center gap-2 rounded-xl border-[1.25px] border-ink bg-hl-mint/35 px-3 py-2.5">
            <a
              href={shortUrl}
              target="_blank"
              rel="noreferrer"
              data-testid="created-short-url"
              className="min-w-0 flex-1 truncate font-mono text-sm font-bold text-ink hover:underline"
            >
              {shortUrl}
            </a>
            <ExternalLink className="size-3.5 shrink-0 text-ink-secondary" />
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
          <Button variant="highlight" onClick={onClose}>
            完成
          </Button>
        </DialogFooter>
      </>
    );
  }

  const showHttpsHint = needsHttpsPrefix(url);

  return (
    <>
      <DialogHeader>
        <DialogTitle>{isEdit ? `编辑短链 /r/${editingLink.code}` : '创建短链'}</DialogTitle>
        <DialogDescription className={isEdit ? undefined : 'sr-only'}>
          {isEdit ? '保存后新网址立即生效，系统将自动复检安全性。' : '填写目标网址与有效期。'}
        </DialogDescription>
      </DialogHeader>

      <form className="space-y-4" onSubmit={handleSubmit} noValidate>
        <div className="space-y-2">
          <Label htmlFor="link-url">目标网址</Label>
          <Input
            id="link-url"
            type="url"
            required
            autoFocus
            placeholder="https://example.com/landing?ref=ori"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
          />
          {showHttpsHint && (
            <div className="flex items-center justify-between gap-2 rounded-lg border border-ink/30 bg-hl-yellow/35 px-2.5 py-1.5 text-xs text-ink">
              <span>检测到未带协议头，需要补全为 https:// 吗？</span>
              <button
                type="button"
                onClick={() => setUrl(`https://${url.trim()}`)}
                className="inline-flex shrink-0 items-center gap-1 rounded-md border border-ink bg-card px-2 py-0.5 font-medium hover:bg-hl-yellow cursor-pointer"
              >
                <Sparkles className="size-3" />
                补全协议
              </button>
            </div>
          )}
        </div>

        <ExpirationPicker
          id="link-expires"
          mode={isEdit ? 'edit' : 'create'}
          initialIso={editingLink?.expires_at}
          value={expiration}
          onChange={setExpiration}
        />

        {formError && <ErrorAlert compact message={formError} />}

        <DialogFooter>
          <Button variant="outline" disabled={isPending} onClick={onClose}>
            取消
          </Button>
          <Button type="submit" variant="highlight" disabled={isPending}>
            {isPending ? '保存中…' : isEdit ? '保存修改' : '生成短链'}
          </Button>
        </DialogFooter>
      </form>
    </>
  );
}
