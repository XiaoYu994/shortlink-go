import { type FormEvent, useEffect, useState } from 'react';
import { Smartphone } from 'lucide-react';
import { useNavigate } from 'react-router';
import { toast } from 'sonner';
import { ErrorAlert } from '@/components/ErrorAlert';
import { Button } from '@/components/ui/button';
import { Input, Label } from '@/components/ui/input';
import { getErrorMessage } from '@/lib/api-client';
import { useSendPhoneOTP, useVerifyPhoneOTP } from './api';

interface PhoneOTPLoginFormProps {
  id: string;
  sendCodeUrl: string;
  verifyCodeUrl: string;
  returnTo: string;
}

export function PhoneOTPLoginForm({
  id,
  sendCodeUrl,
  verifyCodeUrl,
  returnTo,
}: PhoneOTPLoginFormProps) {
  const navigate = useNavigate();
  const [phone, setPhone] = useState('');
  const [code, setCode] = useState('');
  const [cooldown, setCooldown] = useState(0);
  const [formError, setFormError] = useState<string | null>(null);

  const sendOTP = useSendPhoneOTP();
  const verifyOTP = useVerifyPhoneOTP();

  const coolingDown = cooldown > 0;
  useEffect(() => {
    if (!coolingDown) return;
    const timer = window.setInterval(() => {
      setCooldown((prev) => (prev <= 1 ? 0 : prev - 1));
    }, 1000);
    return () => window.clearInterval(timer);
  }, [coolingDown]);

  async function handleSendCode() {
    const trimmed = phone.trim();
    if (!trimmed) {
      setFormError('请输入包含国家区号的手机号（如 +8613800138000）');
      return;
    }
    setFormError(null);
    try {
      const res = await sendOTP.mutateAsync({ url: sendCodeUrl, phone: trimmed });
      setCooldown(Math.max(res.cooldown_seconds, 1));
      toast.success('验证码已发送，请注意查收短信');
    } catch (err) {
      setFormError(getErrorMessage(err));
    }
  }

  async function handleVerify(e: FormEvent) {
    e.preventDefault();
    const trimmedPhone = phone.trim();
    const trimmedCode = code.trim();
    if (!trimmedPhone) {
      setFormError('请输入包含国家区号的手机号（如 +8613800138000）');
      return;
    }
    if (!/^\d{6}$/.test(trimmedCode)) {
      setFormError('请输入 6 位数字验证码');
      return;
    }
    setFormError(null);
    try {
      await verifyOTP.mutateAsync({
        url: verifyCodeUrl,
        phone: trimmedPhone,
        code: trimmedCode,
      });
      navigate(returnTo, { replace: true });
    } catch (err) {
      setFormError(getErrorMessage(err));
    }
  }

  const phoneInputId = `phone-otp-number-${id}`;
  const codeInputId = `phone-otp-code-${id}`;

  return (
    <form className="space-y-3" onSubmit={handleVerify} noValidate>
      <div className="space-y-1.5">
        <Label htmlFor={phoneInputId}>手机号（E.164 格式）</Label>
        <div className="flex gap-2">
          <Input
            id={phoneInputId}
            type="tel"
            autoComplete="tel"
            placeholder="+8613800138000"
            value={phone}
            onChange={(e) => setPhone(e.target.value)}
          />
          <Button
            type="button"
            variant="outline"
            className="shrink-0"
            disabled={sendOTP.isPending || cooldown > 0}
            onClick={() => void handleSendCode()}
          >
            {sendOTP.isPending ? '发送中…' : cooldown > 0 ? `${cooldown}s 后重试` : '获取验证码'}
          </Button>
        </div>
      </div>

      <div className="space-y-1.5">
        <Label htmlFor={codeInputId}>短信验证码</Label>
        <Input
          id={codeInputId}
          type="text"
          inputMode="numeric"
          autoComplete="one-time-code"
          maxLength={6}
          placeholder="输入 6 位数字验证码"
          value={code}
          onChange={(e) => setCode(e.target.value)}
        />
      </div>

      {formError && <ErrorAlert compact message={formError} />}

      <Button type="submit" className="w-full" disabled={verifyOTP.isPending}>
        <Smartphone className="size-4" />
        {verifyOTP.isPending ? '正在验证…' : '使用手机号登录'}
      </Button>
    </form>
  );
}
