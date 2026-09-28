// Single source for how a run receipt is presented. The verification command
// result (passed/failed/not-run) and the evidence gate's terminal state
// (verified/partially-verified/unverified/blocked) are different signals: a
// blocked run whose test command passed must never read as a success, and a
// terminal state alone never implies that verification passed.

export type ReceiptTone = 'success' | 'warning' | 'danger' | 'neutral';

export interface ReceiptVerdictInput {
  terminalState?: string;
  verificationStatus?: string;
  stopReason?: string;
  completionBlocked?: boolean;
}

export interface ReceiptVerdict {
  label: string;
  tone: ReceiptTone;
  detail?: string;
}

const BLOCKING_STOP_REASONS = new Set(['completion_blocked', 'max_cycles']);

export function describeReceipt(input: ReceiptVerdictInput): ReceiptVerdict {
  const terminal = (input.terminalState || '').toLowerCase();
  const verification = (input.verificationStatus || '').toLowerCase();
  const stop = (input.stopReason || '').toLowerCase();
  const verificationDetail = verification ? `verification ${verification}` : undefined;

  if (input.completionBlocked || terminal === 'blocked' || BLOCKING_STOP_REASONS.has(stop)) {
    return { label: 'Blocked', tone: 'danger', detail: stop && stop !== 'blocked' ? stop : verificationDetail };
  }
  if (verification === 'failed' || terminal === 'failed') {
    return { label: 'Failed', tone: 'danger', detail: terminal && terminal !== 'failed' ? terminal : undefined };
  }
  if (terminal === 'verified') {
    return { label: 'Verified', tone: 'success', detail: verificationDetail };
  }
  if (terminal === 'partially-verified') {
    return { label: 'Partially verified', tone: 'warning', detail: verificationDetail };
  }
  if (terminal === 'unverified') {
    return { label: 'Unverified', tone: 'neutral', detail: verificationDetail };
  }
  if (verification === 'passed') {
    return { label: 'Passed', tone: 'success' };
  }
  return { label: 'Not run', tone: 'neutral', detail: terminal || undefined };
}

export const receiptToneClass: Record<ReceiptTone, string> = {
  success: 'text-[var(--color-green)] bg-[var(--color-green)]/10 border-[var(--color-green)]/25',
  warning: 'text-[var(--color-warning)] bg-[var(--color-warning)]/10 border-[var(--color-warning)]/30',
  danger: 'text-[var(--color-red)] bg-[var(--color-red)]/10 border-[var(--color-red)]/25',
  neutral: 'text-[var(--color-text2)] bg-[var(--color-surface2)] border-[var(--color-border)]',
};
