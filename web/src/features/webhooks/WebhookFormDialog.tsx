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
import { MAX_WEBHOOK_DESCRIPTION_BYTES } from '@/lib/constants';
import {
  ALL_WEBHOOK_EVENTS,
  useCreateWebhook,
  useUpdateWebhook,
  type WebhookEvent,
  type WebhookSubscription,
  type WebhookWithSecret,
} from './api';

const utf8Encoder = new TextEncoder();

interface WebhookFormDialogProps {
  orgId: number;
  open: boolean;
  editing: WebhookSubscription | null;
  onOpenChange: (open: boolean) => void;
  onCreated: (created: WebhookWithSecret) => void;
}

export function WebhookFormDialog({
  orgId,
  open,
  editing,
  onOpenChange,
  onCreated,
}: WebhookFormDialogProps) {
  const isEditing = editing !== null;
  const [url, setUrl] = useState(editing?.url ?? '');
  const [description, setDescription] = useState(editing?.description ?? '');
  const [events, setEvents] = useState<WebhookEvent[]>(
    editing?.events ?? ALL_WEBHOOK_EVENTS.map((item) => item.value),
  );
  const [formError, setFormError] = useState<string | null>(null);

  const createWebhook = useCreateWebhook(orgId);
  const updateWebhook = useUpdateWebhook(orgId);
  const isPending = createWebhook.isPending || updateWebhook.isPending;

  function toggleEvent(ev: WebhookEvent) {
    setEvents((prev) => (prev.includes(ev) ? prev.filter((item) => item !== ev) : [...prev, ev]));
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const trimmedUrl = url.trim();
    const trimmedDesc = description.trim();
    if (!trimmedUrl) {
      setFormError('请输入接收通知的网址');
      return;
    }
    if (utf8Encoder.encode(trimmedDesc).length > MAX_WEBHOOK_DESCRIPTION_BYTES) {
      setFormError('备注过长，请精简后再试');
      return;
    }
    if (events.length === 0) {
      setFormError('请至少选择一个订阅事件');
      return;
    }
    setFormError(null);
    try {
      if (isEditing) {
        await updateWebhook.mutateAsync({
          id: editing.id,
          url: trimmedUrl,
          description: trimmedDesc,
          events,
        });
        toast.success(`已更新 Webhook #${editing.id}`);
        onOpenChange(false);
      } else {
        const created = await createWebhook.mutateAsync({
          url: trimmedUrl,
          ...(trimmedDesc ? { description: trimmedDesc } : {}),
          events,
        });
        toast.success(`已创建 Webhook #${created.id}`);
        onOpenChange(false);
        onCreated(created);
      }
    } catch (err) {
      setFormError(getErrorMessage(err));
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>
            {isEditing ? `编辑 Webhook #${editing.id}` : '添加 Webhook 订阅'}
          </DialogTitle>
          <DialogDescription>建议使用 HTTPS 地址，不支持重定向与内网地址。</DialogDescription>
        </DialogHeader>
        <form className="space-y-4" onSubmit={handleSubmit}>
          <div className="space-y-2">
            <Label htmlFor="webhook-url">接收通知地址</Label>
            <Input
              id="webhook-url"
              type="url"
              placeholder="https://hooks.example.com/shortlink"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
            />
          </div>

          <div className="space-y-2">
            <Label htmlFor="webhook-description">备注（可选）</Label>
            <Input
              id="webhook-description"
              placeholder="例如：生产安全风控同步、BI 事件入库"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
            />
          </div>

          <div className="space-y-2">
            <span className="block text-sm font-medium leading-none text-ink">订阅事件</span>
            <div className="space-y-2 rounded-xl border-[1.25px] border-ink/60 bg-paper-warm/50 p-3">
              {ALL_WEBHOOK_EVENTS.map((item) => (
                <label key={item.value} className="flex items-start gap-2.5 text-sm cursor-pointer">
                  <input
                    type="checkbox"
                    className="mt-0.5 size-4 rounded border-ink accent-ink"
                    checked={events.includes(item.value)}
                    onChange={() => toggleEvent(item.value)}
                  />
                  <span>
                    <span className="font-mono text-xs font-bold text-ink">{item.label}</span>
                    <span className="block text-xs text-ink-secondary">{item.description}</span>
                  </span>
                </label>
              ))}
            </div>
          </div>

          {formError && <ErrorAlert compact message={formError} />}

          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              取消
            </Button>
            <Button type="submit" variant="highlight" disabled={isPending}>
              {isPending ? '保存中…' : isEditing ? '保存修改' : '确认创建'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
