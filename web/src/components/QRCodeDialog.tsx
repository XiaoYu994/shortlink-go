import { ExternalLink } from 'lucide-react';
import { CopyButton } from './CopyButton';
import { QRCodeCard } from './QRCodeCard';
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from './ui/dialog';

interface QRCodeDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  code: string;
  shortUrl: string;
  targetUrl?: string;
}

export function QRCodeDialog({ open, onOpenChange, code, shortUrl, targetUrl }: QRCodeDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-sm">
        <DialogHeader>
          <DialogTitle>短链二维码</DialogTitle>
          <DialogDescription>
            扫码即可直接访问该短链，适合海报、PPT 与移动端分享。
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          <QRCodeCard url={shortUrl} code={code} />

          <div className="space-y-1.5">
            <div className="flex items-center gap-2 rounded-lg border border-zinc-200 bg-zinc-50 px-3 py-2">
              <a
                href={shortUrl}
                target="_blank"
                rel="noreferrer"
                className="min-w-0 flex-1 truncate font-mono text-xs font-medium text-brand hover:underline"
              >
                {shortUrl}
              </a>
              <ExternalLink className="size-3.5 shrink-0 text-zinc-400" />
              <CopyButton value={shortUrl} label="复制短链" toastMessage={`已复制 ${shortUrl}`} />
            </div>
            {targetUrl && (
              <p className="truncate text-xs text-zinc-400" title={targetUrl}>
                目标：{targetUrl}
              </p>
            )}
          </div>
        </div>
      </DialogContent>
    </Dialog>
  );
}
