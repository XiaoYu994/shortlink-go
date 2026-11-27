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
          <DialogDescription className="sr-only">
            短链 {code} 二维码与复制分享入口。
          </DialogDescription>
        </DialogHeader>

        <div className="min-w-0 space-y-4">
          <QRCodeCard url={shortUrl} code={code} />

          <div className="min-w-0 space-y-1.5">
            <div className="flex min-w-0 items-center gap-2 rounded-lg border-[1.25px] border-ink/75 bg-paper-warm/60 px-3 py-2">
              <a
                href={shortUrl}
                target="_blank"
                rel="noreferrer"
                className="min-w-0 flex-1 truncate font-mono text-xs font-semibold text-ink underline decoration-hl-yellow decoration-2 underline-offset-2 hover:bg-hl-yellow/40"
              >
                {shortUrl}
              </a>
              <ExternalLink className="size-3.5 shrink-0 text-ink-secondary" />
              <CopyButton value={shortUrl} label="复制短链" toastMessage={`已复制 ${shortUrl}`} />
            </div>
            {targetUrl && (
              <p className="truncate text-xs text-muted-foreground" title={targetUrl}>
                目标：{targetUrl}
              </p>
            )}
          </div>
        </div>
      </DialogContent>
    </Dialog>
  );
}
