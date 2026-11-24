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
import { MAX_ORG_NAME_BYTES, MAX_OWNED_ORGS } from '@/lib/constants';
import { type CreatedOrg, useCreateOrg } from './api';

interface CreateOrgDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: (org: CreatedOrg) => void;
}

export function CreateOrgDialog({ open, onOpenChange, onCreated }: CreateOrgDialogProps) {
  const [name, setName] = useState('');
  const [formError, setFormError] = useState<string | null>(null);
  const createOrg = useCreateOrg();

  function handleOpenChange(nextOpen: boolean) {
    if (!nextOpen) {
      setName('');
      setFormError(null);
    }
    onOpenChange(nextOpen);
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const trimmed = name.trim();
    if (!trimmed) {
      setFormError('请输入组织名称');
      return;
    }
    setFormError(null);
    try {
      const created = await createOrg.mutateAsync(trimmed);
      toast.success(`已创建组织「${created.name}」`);
      handleOpenChange(false);
      onCreated(created);
    } catch (err) {
      setFormError(getErrorMessage(err));
    }
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>新建组织</DialogTitle>
          <DialogDescription>
            创建后你将自动成为该组织的 Owner，拥有独立的短链、成员与 API Key 隔离空间。
          </DialogDescription>
        </DialogHeader>
        <form className="space-y-4" onSubmit={handleSubmit}>
          <div className="space-y-2">
            <Label htmlFor="create-org-name">组织名称</Label>
            <Input
              id="create-org-name"
              placeholder="例如：Acme Corp"
              maxLength={MAX_ORG_NAME_BYTES}
              autoFocus
              required
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
            <p className="text-xs text-muted-foreground">
              每个账号最多作为 Owner 拥有 {MAX_OWNED_ORGS} 个组织。
            </p>
          </div>
          {formError && <ErrorAlert compact message={formError} />}
          <DialogFooter>
            <Button
              variant="outline"
              disabled={createOrg.isPending}
              onClick={() => handleOpenChange(false)}
            >
              取消
            </Button>
            <Button type="submit" disabled={createOrg.isPending}>
              {createOrg.isPending ? '创建中…' : '创建组织'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
