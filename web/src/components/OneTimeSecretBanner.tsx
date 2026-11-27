import { type ReactNode, useState } from 'react';
import { Eye, EyeOff, ShieldAlert } from 'lucide-react';
import { CopyButton } from './CopyButton';
import { Button } from './ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from './ui/card';

function maskSecret(secret: string): string {
  if (secret.length <= 12) return '•'.repeat(secret.length);
  return `${secret.slice(0, 8)}${'•'.repeat(14)}${secret.slice(-4)}`;
}

interface OneTimeSecretBannerProps {
  title: string;
  description: ReactNode;
  secret: string;
  secretTestId?: string;
  copyLabel?: string;
  copyToastMessage?: string;
  onDismiss: () => void;
  children?: ReactNode;
}

/*
  一次性明文密钥安全横幅（Stripe / Resend 模式）：
  默认掩码防投屏泄露，点击复制无需展露明文，并要求勾选确认已保存后才允许关闭。
*/
export function OneTimeSecretBanner({
  title,
  description,
  secret,
  secretTestId,
  copyLabel = '复制密钥',
  copyToastMessage = '已复制明文密钥',
  onDismiss,
  children,
}: OneTimeSecretBannerProps) {
  const [revealed, setRevealed] = useState(false);
  const [acknowledged, setAcknowledged] = useState(false);

  return (
    <Card className="border-[1.5px] border-ink bg-[#FFFBEB] ink-shadow-sm">
      <CardHeader className="flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div className="flex items-start gap-2.5">
          <span className="mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-lg border-[1.25px] border-ink bg-hl-yellow text-ink">
            <ShieldAlert className="size-4" />
          </span>
          <div className="space-y-1">
            <CardTitle className="text-ink">{title}</CardTitle>
            <CardDescription className="text-ink-secondary">{description}</CardDescription>
          </div>
        </div>
        <Button
          variant="outline"
          size="sm"
          disabled={!acknowledged}
          onClick={onDismiss}
          className="shrink-0"
        >
          我已保存，关闭
        </Button>
      </CardHeader>

      <CardContent className="space-y-3">
        <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
          <code
            data-testid={secretTestId}
            className="flex-1 break-all rounded-lg border-[1.25px] border-ink/80 bg-card px-3 py-2 font-mono text-xs text-ink"
          >
            {revealed ? secret : maskSecret(secret)}
          </code>
          <div className="flex shrink-0 items-center gap-1.5">
            <Button
              variant="outline"
              size="sm"
              aria-label={revealed ? '隐藏明文密钥' : '显示明文密钥'}
              onClick={() => setRevealed((prev) => !prev)}
            >
              {revealed ? <EyeOff className="size-3.5" /> : <Eye className="size-3.5" />}
              {revealed ? '隐藏' : '显示明文'}
            </Button>
            <CopyButton
              value={secret}
              variant="pill"
              size="sm"
              label={copyLabel}
              toastMessage={copyToastMessage}
            />
          </div>
        </div>

        <label className="inline-flex items-center gap-2 text-xs font-medium text-ink cursor-pointer select-none">
          <input
            type="checkbox"
            className="size-4 rounded border-ink accent-ink"
            checked={acknowledged}
            onChange={(e) => setAcknowledged(e.target.checked)}
          />
          我已安全复制并保存该密钥（关闭后将无法再次查看）
        </label>

        {children}
      </CardContent>
    </Card>
  );
}
