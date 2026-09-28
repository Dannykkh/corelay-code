import { CircleAlert, CircleCheck, CircleX, MinusCircle } from 'lucide-react';
import { describeReceipt, receiptToneClass, type ReceiptVerdictInput } from '../lib/receipts';

const TONE_ICON = {
  success: CircleCheck,
  warning: CircleAlert,
  danger: CircleX,
  neutral: MinusCircle,
} as const;

export function ReceiptBadge({ receipt, showDetail = true }: { receipt: ReceiptVerdictInput; showDetail?: boolean }) {
  const verdict = describeReceipt(receipt);
  const Icon = TONE_ICON[verdict.tone];
  return (
    <span className="inline-flex items-center gap-1.5">
      <span
        data-receipt-tone={verdict.tone}
        className={`inline-flex items-center gap-1 text-[11px] font-medium px-2 py-0.5 rounded border ${receiptToneClass[verdict.tone]}`}
      >
        <Icon aria-hidden="true" className="w-3.5 h-3.5" />
        {verdict.label}
      </span>
      {showDetail && verdict.detail && (
        <span className="text-[11px] font-mono text-[var(--color-text2)]">{verdict.detail}</span>
      )}
    </span>
  );
}
