import { useEffect, useState } from 'react';
import { Download } from 'lucide-react';
import QRCode from 'qrcode';
import { Button } from './ui/button';
import { Skeleton } from './ui/skeleton';

interface QRCodeCardProps {
  url: string;
  code: string;
}

/*
  纯本地离线生成短链二维码（零第三方图片请求），并支持一键下载 PNG。
*/
export function QRCodeCard({ url, code }: QRCodeCardProps) {
  const [dataUrl, setDataUrl] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let active = true;
    QRCode.toDataURL(url, {
      width: 220,
      margin: 2,
      color: { dark: '#18181b', light: '#ffffff' },
    })
      .then((res) => {
        if (active) {
          setDataUrl(res);
          setFailed(false);
        }
      })
      .catch(() => {
        if (active) {
          setDataUrl(null);
          setFailed(true);
        }
      });
    return () => {
      active = false;
    };
  }, [url]);

  function handleDownload() {
    if (!dataUrl) return;
    const a = document.createElement('a');
    a.href = dataUrl;
    a.download = `shortlink-${code}.png`;
    a.click();
  }

  return (
    <div className="flex flex-col items-center gap-3 rounded-xl border border-zinc-200/90 bg-zinc-50/50 p-4">
      {failed ? (
        <div className="flex size-44 items-center justify-center rounded-lg border border-zinc-200 bg-white text-xs text-zinc-400">
          二维码生成失败
        </div>
      ) : dataUrl ? (
        <img
          src={dataUrl}
          alt={`短链 ${code} 二维码`}
          className="size-44 rounded-lg border border-zinc-200 bg-white p-1.5 shadow-2xs"
        />
      ) : (
        <Skeleton className="size-44 rounded-lg" />
      )}
      <Button variant="outline" size="sm" disabled={!dataUrl} onClick={handleDownload}>
        <Download className="size-3.5" />
        下载 PNG 二维码
      </Button>
    </div>
  );
}
