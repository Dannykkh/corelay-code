import { useState } from 'react';
import {
  ChevronRight,
  ChevronDown,
  CircleCheck,
  CircleX,
  LoaderCircle,
  MinusCircle,
  Terminal,
  FileCode,
  Search,
  Wrench,
  ShieldAlert,
  Check,
  X,
} from 'lucide-react';
import type { ApprovalDecision } from '../lib/api';

export interface ActiveApproval {
  id: string;
  runtimeSessionId: string;
  toolName: string;
  toolCallId?: string;
  redactedInput: string;
  dangerLevel?: string;
  scope?: string;
  expiresAt?: string;
  state: 'pending' | 'submitting' | 'error' | 'resolved' | 'expired';
  decision?: ApprovalDecision;
  error?: string;
}

export interface StepCardProps {
  toolName?: string;
  toolInput?: Record<string, unknown> | string;
  toolResult?: string;
  toolDiff?: string;
  isError?: boolean;
  timestamp: Date;
  /** The approval linked to this exact tool call, if any. Chat does the linking. */
  activeApproval?: ActiveApproval | null;
  onResolveApproval?: (decision: ApprovalDecision) => Promise<void>;
  denyRef?: React.Ref<HTMLButtonElement>;
  defaultExpanded?: boolean;
  /** False once the run ended; a card still without a result then stops spinning. */
  runActive?: boolean;
}

function extractToolTarget(input?: Record<string, unknown> | string): string {
  if (!input) return '';
  let obj: Record<string, unknown> | null = null;
  if (typeof input === 'string') {
    try {
      obj = JSON.parse(input) as Record<string, unknown>;
    } catch {
      return input.length > 50 ? input.slice(0, 50) + '…' : input;
    }
  } else {
    obj = input;
  }
  if (!obj) return '';
  if (typeof obj.file_path === 'string') return obj.file_path;
  if (typeof obj.path === 'string') return obj.path;
  if (typeof obj.command === 'string') {
    const cmd = obj.command.trim();
    return cmd.length > 60 ? cmd.slice(0, 60) + '…' : cmd;
  }
  if (typeof obj.pattern === 'string') return obj.pattern;
  if (typeof obj.query === 'string') return obj.query;
  if (typeof obj.url === 'string') return obj.url;
  return '';
}

function toolIcon(name?: string) {
  const n = (name || '').toLowerCase();
  if (n === 'bash' || n === 'sh' || n === 'cmd' || n === 'git') {
    return <Terminal aria-hidden="true" className="w-3.5 h-3.5" />;
  }
  if (n === 'read' || n === 'write' || n === 'edit' || n.includes('file')) {
    return <FileCode aria-hidden="true" className="w-3.5 h-3.5" />;
  }
  if (n === 'grep' || n === 'glob' || n.includes('search')) {
    return <Search aria-hidden="true" className="w-3.5 h-3.5" />;
  }
  return <Wrench aria-hidden="true" className="w-3.5 h-3.5" />;
}

function approvalOutcomeLabel(approval: ActiveApproval): string {
  if (approval.state === 'expired') return 'Approval expired; nothing was allowed';
  return approval.decision === 'deny' ? 'Denied' : 'Allowed once';
}

export function StepCard({
  toolName = 'Tool',
  toolInput,
  toolResult,
  toolDiff,
  isError,
  timestamp,
  activeApproval,
  onResolveApproval,
  denyRef,
  defaultExpanded = false,
  runActive = true,
}: StepCardProps) {
  const hasResult = toolResult !== undefined;
  const isPendingApproval = Boolean(
    activeApproval &&
    !hasResult &&
    (activeApproval.state === 'pending' || activeApproval.state === 'submitting' || activeApproval.state === 'error')
  );
  const settledApproval = activeApproval && !hasResult &&
    (activeApproval.state === 'resolved' || activeApproval.state === 'expired')
    ? activeApproval
    : null;
  const blockedByApproval = settledApproval != null &&
    (settledApproval.state === 'expired' || settledApproval.decision === 'deny');

  const [userExpanded, setUserExpanded] = useState<boolean | null>(null);
  // A pending approval always keeps its card open so the decision is visible.
  const expanded = isPendingApproval || (userExpanded ?? defaultExpanded);

  const target = extractToolTarget(toolInput);
  const isRunning = !hasResult && !isError && !isPendingApproval && !blockedByApproval && runActive;
  const endedWithoutResult = !hasResult && !isPendingApproval && !blockedByApproval && !runActive;

  return (
    <div
      data-pending-approval={isPendingApproval ? 'true' : undefined}
      className="mx-2 my-1 border border-[var(--color-border)] rounded-lg bg-[var(--color-surface)] overflow-hidden transition-all text-xs"
    >
      {/* Collapsed One-line Summary Header */}
      <button
        type="button"
        onClick={() => {
          if (!isPendingApproval) setUserExpanded(!expanded);
        }}
        aria-expanded={expanded}
        aria-disabled={isPendingApproval || undefined}
        className="w-full px-3 py-2 flex items-center justify-between gap-2.5 text-left bg-[var(--color-surface2)]/40 hover:bg-[var(--color-surface2)]/70 transition-colors"
      >
        <div className="flex items-center gap-2 min-w-0">
          {/* Status Icon */}
          <span className="flex-shrink-0">
            {isRunning ? (
              <LoaderCircle aria-hidden="true" className="w-3.5 h-3.5 text-[var(--color-accent)] animate-spin" />
            ) : isError || blockedByApproval ? (
              <CircleX aria-hidden="true" className="w-3.5 h-3.5 text-[var(--color-red)]" />
            ) : isPendingApproval ? (
              <ShieldAlert aria-hidden="true" className="w-3.5 h-3.5 text-[var(--color-warning)] animate-pulse" />
            ) : endedWithoutResult ? (
              <MinusCircle aria-label="no result" className="w-3.5 h-3.5 text-[var(--color-text2)]" />
            ) : (
              <CircleCheck aria-hidden="true" className="w-3.5 h-3.5 text-[var(--color-green)]" />
            )}
          </span>

          {/* Tool icon & Tool Name */}
          <span className="flex items-center gap-1 font-semibold text-[var(--color-accent)] flex-shrink-0">
            {toolIcon(toolName)}
            <span>{toolName}</span>
          </span>

          {/* Target Summary */}
          {target && (
            <span className="font-mono text-[11px] text-[var(--color-text2)] truncate" title={target}>
              {target}
            </span>
          )}

          {isPendingApproval && (
            <span className="text-[10px] px-1.5 py-0.5 rounded border border-[var(--color-warning)]/50 text-[var(--color-warning)] font-medium">
              Approval required
            </span>
          )}

          {settledApproval && (
            <span className="text-[10px] px-1.5 py-0.5 rounded bg-[var(--color-surface2)] text-[var(--color-text2)] font-medium">
              {approvalOutcomeLabel(settledApproval)}
            </span>
          )}

          {isError && (
            <span className="text-[10px] px-1.5 py-0.5 rounded bg-[var(--color-red)]/15 text-[var(--color-red)] font-medium">
              Error
            </span>
          )}
        </div>

        <div className="flex items-center gap-2 flex-shrink-0 text-[10px] text-[var(--color-text2)]">
          <span>{timestamp.toLocaleTimeString()}</span>
          {expanded ? <ChevronDown aria-hidden="true" className="w-3.5 h-3.5" /> : <ChevronRight aria-hidden="true" className="w-3.5 h-3.5" />}
        </div>
      </button>

      {/* Expanded Details Body */}
      {expanded && (
        <div className="p-3 border-t border-[var(--color-border)] space-y-2.5 bg-[var(--color-bg)]">
          {/* Active Approval Panel (Embedded) */}
          {isPendingApproval && activeApproval && onResolveApproval && (
            <div
              role="alert"
              aria-busy={activeApproval.state === 'submitting'}
              className="p-3 rounded-lg border border-[var(--color-warning)]/40 bg-[var(--color-surface)] space-y-2"
            >
              <div className="flex items-center justify-between gap-2">
                <div className="flex items-center gap-1.5">
                  <ShieldAlert aria-hidden="true" className="w-4 h-4 text-[var(--color-warning)]" />
                  <span className="font-semibold text-xs text-[var(--color-text)]">
                    Permission required
                  </span>
                  {activeApproval.dangerLevel && (
                    <span className="text-[10px] px-1.5 py-0.5 rounded font-mono border border-[var(--color-warning)]/50 text-[var(--color-warning)]">
                      {activeApproval.dangerLevel}
                    </span>
                  )}
                </div>

                <div className="flex items-center gap-1.5">
                  <button
                    type="button"
                    onClick={() => void onResolveApproval('allow_once')}
                    disabled={activeApproval.state === 'submitting'}
                    className="flex items-center gap-1 px-2.5 py-1 border border-[var(--color-green)] text-[var(--color-text)] rounded hover:bg-[var(--color-green)]/10 disabled:opacity-50 text-xs font-medium"
                  >
                    <Check aria-hidden="true" className="w-3.5 h-3.5 text-[var(--color-green)]" />
                    Allow once
                  </button>
                  <button
                    ref={denyRef}
                    type="button"
                    onClick={() => void onResolveApproval('deny')}
                    disabled={activeApproval.state === 'submitting'}
                    className="flex items-center gap-1 px-2.5 py-1 border border-[var(--color-red)] text-[var(--color-text)] rounded hover:bg-[var(--color-red)]/10 disabled:opacity-50 text-xs font-medium"
                  >
                    <X aria-hidden="true" className="w-3.5 h-3.5 text-[var(--color-red)]" />
                    Deny
                  </button>
                </div>
              </div>

              {activeApproval.state === 'error' && (
                <div className="text-[11px] text-[var(--color-red)] bg-[var(--color-red)]/10 p-2 rounded">
                  {activeApproval.error || 'Approval could not be recorded. Nothing was approved; retry or deny.'}
                </div>
              )}

              <pre className="font-mono text-[11px] p-2 rounded bg-[var(--color-bg)] border border-[var(--color-border)] text-[var(--color-text)] max-h-32 overflow-auto whitespace-pre-wrap break-all">
                {activeApproval.redactedInput}
              </pre>

              {activeApproval.expiresAt && (
                <div className="text-[10px] text-[var(--color-text2)]">
                  Expires at {new Date(activeApproval.expiresAt).toLocaleTimeString()}
                </div>
              )}
            </div>
          )}

          {/* Input Block */}
          {toolInput && (
            <div className="space-y-1">
              <div className="text-[10px] uppercase font-semibold text-[var(--color-text2)]">Input</div>
              <pre className="p-2 rounded bg-[var(--color-surface)] border border-[var(--color-border)] font-mono text-[11px] text-[var(--color-text)] max-h-48 overflow-auto whitespace-pre-wrap break-all leading-relaxed">
                {typeof toolInput === 'string' ? toolInput : JSON.stringify(toolInput, null, 2)}
              </pre>
            </div>
          )}

          {/* Output Block */}
          {hasResult ? (
            <div className="space-y-1">
              <div className="text-[10px] uppercase font-semibold text-[var(--color-text2)]">Output</div>
              <pre
                className={`p-2 rounded bg-[var(--color-surface)] border border-[var(--color-border)] font-mono text-[11px] max-h-60 overflow-auto whitespace-pre-wrap break-all leading-relaxed ${
                  isError ? 'text-[var(--color-red)]' : 'text-[var(--color-text)]'
                }`}
              >
                {toolResult}
              </pre>
            </div>
          ) : isRunning ? (
            <div className="flex items-center gap-2 py-1 text-[var(--color-text2)]">
              <LoaderCircle aria-hidden="true" className="w-3.5 h-3.5 text-[var(--color-accent)] animate-spin" />
              <span>Executing tool…</span>
            </div>
          ) : blockedByApproval && settledApproval ? (
            <div className="py-1 text-[var(--color-text2)]">Not executed: {approvalOutcomeLabel(settledApproval)}.</div>
          ) : endedWithoutResult ? (
            <div className="py-1 text-[var(--color-text2)]">No result recorded: the run ended before this tool reported back.</div>
          ) : null}

          {/* Diff Block */}
          {toolDiff && (
            <div className="space-y-1">
              <div className="text-[10px] uppercase font-semibold text-[var(--color-text2)]">Diff</div>
              <pre className="p-2 rounded bg-[var(--color-surface)] border border-[var(--color-border)] font-mono text-[11px] max-h-64 overflow-auto whitespace-pre leading-relaxed">
                {toolDiff.split('\n').map((line, k) => {
                  const cls = line.startsWith('+ ')
                    ? 'text-[var(--color-green)] bg-[var(--color-green)]/10'
                    : line.startsWith('- ')
                    ? 'text-[var(--color-red)] bg-[var(--color-red)]/10'
                    : 'text-[var(--color-text2)]';
                  return (
                    <div key={k} className={cls}>
                      {line || ' '}
                    </div>
                  );
                })}
              </pre>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
