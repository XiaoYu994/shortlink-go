import { type FormEvent, useEffect, useRef, useState } from 'react';
import { Smartphone } from 'lucide-react';
import { useNavigate } from 'react-router';
import { toast } from 'sonner';
import { ErrorAlert } from '@/components/ErrorAlert';
import { Button } from '@/components/ui/button';
import { Input, Label, Select } from '@/components/ui/input';
import { getErrorMessage } from '@/lib/api-client';
import { useSendPhoneOTP, useVerifyPhoneOTP } from './api';

const DIAL_CODES = [
  { value: '+86', label: '+86' },
  { value: '+852', label: '+852' },
  { value: '+853', label: '+853' },
  { value: '+886', label: '+886' },
  { value: '+1', label: '+1' },
  { value: '+81', label: '+81' },
  { value: '+44', label: '+44' },
  { value: '+65', label: '+65' },
] as const;

function normalizePhone(dialCode: string, raw: string): string {
  const compact = raw.trim().replace(/[\s-]/g, '');
  if (!compact) return '';
  if (compact.startsWith('+')) {
    return compact;
  }
  const digits = compact.replace(/\D/g, '');
  return digits ? `${dialCode}${digits}` : '';
}

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
  const codeInputRef = useRef<HTMLInputElement>(null);
  const [dialCode, setDialCode] = useState('+86');
  const [phone, setPhone] = useState('');
  const [code, setCode] = useState('');
  const [cooldown, setCooldown] = useState(0);
  const [expiresInSec, setExpiresInSec] = useState<number | null>(null);
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

  const normalizedPhone = normalizePhone(dialCode, phone);

  async function handleSendCode() {
    if (!normalizedPhone) {
      setFormError('请输入有效手机号');
      return;
    }
    setFormError(null);
    try {
      const res = await sendOTP.mutateAsync({ url: sendCodeUrl, phone: normalizedPhone });
      setCooldown(Math.max(res.cooldown_seconds, 1));
      setExpiresInSec(res.expires_in);
      toast.success('验证码已发送，请注意查收短信');
      codeInputRef.current?.focus();
    } catch (err) {
      setFormError(getErrorMessage(err));
    }
  }

  async function handleVerify(e: FormEvent) {
    e.preventDefault();
    const trimmedCode = code.trim();
    if (!normalizedPhone) {
      setFormError('请输入有效手机号');
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
        phone: normalizedPhone,
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
        <Label htmlFor={phoneInputId}>手机号</Label>
        <div className="flex gap-2">
          <Select
            id={`phone-otp-dial-${id}`}
            name="dialCode"
            aria-label="国家区号"
            className="w-[78px] shrink-0 font-mono text-xs"
            value={dialCode}
            onChange={(e) => setDialCode(e.target.value)}
          >
            {DIAL_CODES.map((dc) => (
              <option key={dc.value} value={dc.value}>
                {dc.label}
              </option>
            ))}
          </Select>
          <Input
            id={phoneInputId}
            name="phone"
            type="tel"
            autoComplete="tel"
            placeholder="13800138000"
            className="min-w-0 flex-1"
            value={phone}
            onChange={(e) => setPhone(e.target.value)}
          />
        </div>
      </div>

      <div className="space-y-1.5">
        <div className="flex items-center justify-between gap-2">
          <Label htmlFor={codeInputId}>短信验证码</Label>
          {expiresInSec !== null && (
            <span className="text-[11px] font-medium text-ink-secondary">
              约 {Math.max(1, Math.round(expiresInSec / 60))} 分钟内有效
            </span>
          )}
        </div>
        <div className="flex gap-2">
          <Input
            ref={codeInputRef}
            id={codeInputId}
            name="otpCode"
            type="text"
            inputMode="numeric"
            autoComplete="one-time-code"
            maxLength={6}
            placeholder="输入 6 位数字验证码"
            className="min-w-0 flex-1 font-mono tracking-widest placeholder:font-sans placeholder:tracking-normal"
            value={code}
            onChange={(e) => setCode(e.target.value.replace(/\D/g, '').slice(0, 6))}
          />
          <Button
            type="button"
            variant="outline"
            className="shrink-0"
            disabled={sendOTP.isPending || cooldown > 0 || !normalizedPhone}
            onClick={() => void handleSendCode()}
          >
            {sendOTP.isPending ? '发送中…' : cooldown > 0 ? `${cooldown}s 后重试` : '获取验证码'}
          </Button>
        </div>
      </div>

      {formError && <ErrorAlert compact message={formError} />}

      <Button
        type="submit"
        variant="mint"
        className="w-full"
        disabled={verifyOTP.isPending || !normalizedPhone || !/^\d{6}$/.test(code.trim())}
      >
        <Smartphone className="size-4" />
        {verifyOTP.isPending ? '正在验证…' : '使用手机号登录'}
      </Button>
    </form>
  );
}
