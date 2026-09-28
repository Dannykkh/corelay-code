import { useEffect, useState } from 'react';
import {
  FileText,
  RotateCcw,
  CheckCircle2,
  XCircle,
  HelpCircle,
  ExternalLink,
  ChevronDown,
  ChevronRight,
  Plus,
  RefreshCw,
  X,
  ShieldCheck,
  AlertTriangle,
  FolderGit2,
  FileCode,
} from 'lucide-react';
import type { Workstream, WorkstreamPlan, WorkstreamPlanStage } from '../lib/workstreams';
import { fetchJSON } from '../lib/api';
import { describeReceipt, receiptToneClass, type ReceiptTone } from '../lib/receipts';

const TONE_DOT: Record<ReceiptTone, string> = {
  success: 'bg-[var(--color-green)]',
  warning: 'bg-[var(--color-warning)]',
  danger: 'bg-[var(--color-red)]',
  neutral: 'bg-[var(--color-text2)]',
};

interface ReceiptEvidenceRecord {
  source?: string;
  command?: string;
  status?: string;
  summary?: string;
  exitCode?: number;
}

interface RecentEvidenceItem {
  receiptPath?: string;
  evidence?: ReceiptEvidenceRecord[];
}

export interface CurrentSessionIndicator {
  id: string;
  title: string;
  revision: number;
  lifecycleStatus: string;
}

export type InspectorTab = 'changes' | 'workflow' | 'receipt';

export interface DiffRecord {
  turn: number;
  file: string;
  diff: string;
}

export interface UndoEntry {
  id: string;
  path: string;
  status: string;
}

export interface RunReceipt {
  terminalState?: string;
  kind?: string;
  stopReason?: string;
  completionStatus?: string;
  completionRevision?: number;
  completionBlocked?: boolean;
  verificationStatus?: string;
  receiptPath?: string;
  timestamp?: Date;
}

export interface InspectorProps {
  open: boolean;
  onClose: () => void;
  tab: InspectorTab;
  onTabChange: (tab: InspectorTab) => void;

  // Changes Tab
  changedFiles: string[];
  diffRecords: DiffRecord[];
  currentTurnDiffs: DiffRecord[];
  undoEntries: UndoEntry[];
  undoActionBusy: boolean;
  onCheckRestore: () => void;
  onRestoreEntry: (id: string) => void;

  // Workflow Tab
  workstreams: Workstream[];
  selectedWorkstreamId: string;
  onSelectWorkstream: (id: string) => void;
  onCreateWorkstream: () => void;
  onExportHandoff: () => void;
  cannotClearWorkstream: boolean;

  plans: WorkstreamPlan[];
  selectedPlanId: string;
  selectedPlan?: WorkstreamPlan;
  selectedPlanRevision?: number;
  plansLoading: boolean;
  cannotClearPlan: boolean;
  onSelectPlan: (id: string) => void;

  selectedStageId: string;
  selectedStage?: WorkstreamPlanStage;
  onSelectStage: (id: string) => void;

  planActionBusy: boolean;
  onBindCurrentPlanRevision: () => void;
  onApproveSelectedPlan: () => void;
  onReconcileSelectedPlanStage: () => void;

  currentSession: CurrentSessionIndicator | null;
  sessionNotice: string;
  workstreamNotice: string;

  // Receipt Tab
  latestReceipt: RunReceipt | null;

  // Streaming & Navigation
  streaming?: boolean;
  onNavigateHistory?: (tab?: 'costs' | 'receipts' | 'notifications') => void;
}

function isStageEligible(plan: WorkstreamPlan, stageId: string): boolean {
  const stageIndex = plan.stages.findIndex((stage) => stage.id === stageId);
  if (stageIndex < 0 || plan.stages.some((stage) => stage.status === 'running')) return false;
  const stage = plan.stages[stageIndex];
  if (stage.status !== 'pending' && stage.status !== 'failed') return false;
  return plan.stages.slice(0, stageIndex).every((previous) => previous.status === 'completed');
}

export function Inspector({
  open,
  onClose,
  tab,
  onTabChange,
  changedFiles,
  diffRecords,
  currentTurnDiffs,
  undoEntries,
  undoActionBusy,
  onCheckRestore,
  onRestoreEntry,
  workstreams,
  selectedWorkstreamId,
  onSelectWorkstream,
  onCreateWorkstream,
  onExportHandoff,
  cannotClearWorkstream,
  plans,
  selectedPlanId,
  selectedPlan,
  selectedPlanRevision,
  plansLoading,
  cannotClearPlan,
  onSelectPlan,
  selectedStageId,
  selectedStage,
  onSelectStage,
  planActionBusy,
  onBindCurrentPlanRevision,
  onApproveSelectedPlan,
  onReconcileSelectedPlanStage,
  currentSession,
  sessionNotice,
  workstreamNotice,
  latestReceipt,
  streaming = false,
  onNavigateHistory,
}: InspectorProps) {
  const [expandedDiffs, setExpandedDiffs] = useState<Record<string, boolean>>({});
  const [confirmReconcile, setConfirmReconcile] = useState(false);
  const [receiptEvidence, setReceiptEvidence] = useState<{ path: string; records: ReceiptEvidenceRecord[] } | null>(null);
  const receiptVerdict = latestReceipt ? describeReceipt(latestReceipt) : null;
  const latestReceiptPath = latestReceipt?.receiptPath;

  // The done event carries only the receipt path; the evidence records live in
  // the stored receipt, which the recent-evidence API exposes by path.
  useEffect(() => {
    if (!open || tab !== 'receipt' || !latestReceiptPath || receiptEvidence?.path === latestReceiptPath) return;
    let cancelled = false;
    // scope=all: the receipt belongs to the selected project, which need not be the server default.
    fetchJSON<{ items?: RecentEvidenceItem[] }>('/api/evidence/recent?scope=all&limit=50')
      .then((data) => {
        if (cancelled) return;
        const match = data.items?.find((item) => item.receiptPath === latestReceiptPath);
        setReceiptEvidence({ path: latestReceiptPath, records: match?.evidence ?? [] });
      })
      .catch(() => {
        if (!cancelled) setReceiptEvidence({ path: latestReceiptPath, records: [] });
      });
    return () => {
      cancelled = true;
    };
  }, [open, tab, latestReceiptPath, receiptEvidence?.path]);

  if (!open) return null;

  const toggleDiff = (key: string) => {
    setExpandedDiffs((prev) => ({ ...prev, [key]: !prev[key] }));
  };

  const selectedPlanStage = selectedPlan?.stages.find((s) => s.id === selectedStageId);
  const isStageRunning = selectedPlanStage?.status === 'running';

  return (
    <aside
      className="w-80 md:w-96 border-l border-[var(--color-border)] bg-[var(--color-surface)] flex flex-col h-full flex-shrink-0 z-20 text-xs text-[var(--color-text)] select-text"
      aria-label="Inspector"
    >
      {/* Inspector Header */}
      <div className="px-3 py-2.5 border-b border-[var(--color-border)] flex items-center justify-between bg-[var(--color-surface2)]/50">
        <div className="flex items-center gap-1 font-semibold text-xs text-[var(--color-text)]">
          <FolderGit2 className="w-3.5 h-3.5 text-[var(--color-accent)]" />
          <span>Inspector</span>
        </div>
        <button
          type="button"
          onClick={onClose}
          className="p-1 rounded text-[var(--color-text2)] hover:text-[var(--color-text)] hover:bg-[var(--color-surface)]"
          title="Close Inspector"
          aria-label="인스펙터 닫기"
        >
          <X aria-hidden="true" className="w-4 h-4" />
        </button>
      </div>

      {/* Tabs Bar */}
      <div role="tablist" aria-label="Inspector" className="flex border-b border-[var(--color-border)] bg-[var(--color-bg)]">
        <button
          type="button"
          role="tab"
          id="inspector-tab-changes"
          aria-selected={tab === 'changes'}
          aria-controls="inspector-panel"
          onClick={() => onTabChange('changes')}
          className={`flex-1 py-2 px-2 text-center font-medium border-b-2 transition-colors flex items-center justify-center gap-1.5 ${
            tab === 'changes'
              ? 'border-[var(--color-accent)] text-[var(--color-accent)] bg-[var(--color-surface)]'
              : 'border-transparent text-[var(--color-text2)] hover:text-[var(--color-text)]'
          }`}
        >
          <FileText className="w-3.5 h-3.5" />
          <span>Changes</span>
          {changedFiles.length > 0 && (
            <span className="text-[10px] px-1.5 py-0.5 rounded-full bg-[var(--color-accent)]/15 text-[var(--color-accent)] font-mono">
              {changedFiles.length}
            </span>
          )}
        </button>

        <button
          type="button"
          role="tab"
          id="inspector-tab-workflow"
          aria-selected={tab === 'workflow'}
          aria-controls="inspector-panel"
          onClick={() => onTabChange('workflow')}
          className={`flex-1 py-2 px-2 text-center font-medium border-b-2 transition-colors flex items-center justify-center gap-1.5 ${
            tab === 'workflow'
              ? 'border-[var(--color-accent)] text-[var(--color-accent)] bg-[var(--color-surface)]'
              : 'border-transparent text-[var(--color-text2)] hover:text-[var(--color-text)]'
          }`}
        >
          <RotateCcw className="w-3.5 h-3.5" />
          <span>Workflow</span>
          {(isStageRunning || sessionNotice) && (
            <span aria-label="attention needed" className="w-1.5 h-1.5 rounded-full bg-[var(--color-warning)]" />
          )}
        </button>

        <button
          type="button"
          role="tab"
          id="inspector-tab-receipt"
          aria-selected={tab === 'receipt'}
          aria-controls="inspector-panel"
          onClick={() => onTabChange('receipt')}
          className={`flex-1 py-2 px-2 text-center font-medium border-b-2 transition-colors flex items-center justify-center gap-1.5 ${
            tab === 'receipt'
              ? 'border-[var(--color-accent)] text-[var(--color-accent)] bg-[var(--color-surface)]'
              : 'border-transparent text-[var(--color-text2)] hover:text-[var(--color-text)]'
          }`}
        >
          <ShieldCheck className="w-3.5 h-3.5" />
          <span>Receipt</span>
          {receiptVerdict && (
            <span aria-label={receiptVerdict.label} className={`w-1.5 h-1.5 rounded-full ${TONE_DOT[receiptVerdict.tone]}`} />
          )}
        </button>
      </div>

      {/* Tab Contents */}
      <div id="inspector-panel" role="tabpanel" aria-labelledby={`inspector-tab-${tab}`} className="flex-1 overflow-y-auto p-3 space-y-4">
        {/* ── Tab 1: Changes ── */}
        {tab === 'changes' && (
          <div className="space-y-3">
            {/* Scope Notice */}
            <div className="p-2.5 rounded bg-[var(--color-surface2)] border border-[var(--color-border)] text-[11px] text-[var(--color-text2)] leading-relaxed">
              AI가 편집한 파일만 복원합니다. 명령 부작용은 되돌리지 않습니다.
            </div>

            {/* Changes Summary & Check restore */}
            <div className="flex items-center justify-between">
              <div>
                <span className="font-medium text-[var(--color-text)]">
                  {changedFiles.length} file{changedFiles.length === 1 ? '' : 's'} · {diffRecords.length} diff{diffRecords.length === 1 ? '' : 's'}
                </span>
                {currentTurnDiffs.length > 0 && (
                  <span className="ml-1.5 text-[var(--color-accent)] font-mono text-[10px]">
                    (this turn: {currentTurnDiffs.length})
                  </span>
                )}
              </div>
              <button
                type="button"
                onClick={onCheckRestore}
                disabled={undoActionBusy || streaming}
                className="flex items-center gap-1 px-2 py-1 rounded border border-[var(--color-border)] hover:border-[var(--color-accent)] text-[var(--color-text)] disabled:opacity-50"
              >
                <RefreshCw className={`w-3 h-3 ${undoActionBusy ? 'animate-spin' : ''}`} />
                <span>{undoActionBusy ? 'Checking…' : 'Check restore'}</span>
              </button>
            </div>

            {/* Changed Files List */}
            {changedFiles.length > 0 ? (
              <div className="space-y-1">
                <span className="text-[10px] uppercase font-semibold text-[var(--color-text2)]">Changed Files</span>
                <div className="divide-y divide-[var(--color-border)] border border-[var(--color-border)] rounded-md bg-[var(--color-bg)]">
                  {changedFiles.map((file) => (
                    <div key={file} className="px-2.5 py-1.5 flex items-center gap-2 font-mono text-[11px]">
                      <FileCode className="w-3.5 h-3.5 text-[var(--color-accent)] flex-shrink-0" />
                      <span className="truncate flex-1" title={file}>{file}</span>
                    </div>
                  ))}
                </div>
              </div>
            ) : (
              <div className="py-4 text-center text-[var(--color-text2)]">No changed files in this session</div>
            )}

            {/* Checkpoint Restore Candidates */}
            {undoEntries.length > 0 && (
              <div className="space-y-1.5 pt-2 border-t border-[var(--color-border)]">
                <span className="text-[10px] uppercase font-semibold text-[var(--color-text2)]">Checkpoint Candidates</span>
                <div className="space-y-1.5">
                  {undoEntries.map((entry) => {
                    const restorable = entry.status === 'restorable';
                    return (
                      <div
                        key={entry.id}
                        className="p-2 rounded border border-[var(--color-border)] bg-[var(--color-bg)] flex items-center justify-between gap-2"
                      >
                        <div className="min-w-0 flex-1">
                          <div className="truncate font-mono text-[11px]" title={entry.path}>
                            {entry.path}
                          </div>
                          <div className="text-[10px]">
                            <span
                              className={`px-1 rounded font-mono ${
                                restorable
                                  ? 'bg-[var(--color-green)]/15 text-[var(--color-green)]'
                                  : 'bg-[var(--color-warning)]/15 text-[var(--color-warning)]'
                              }`}
                            >
                              {entry.status}
                            </span>
                          </div>
                        </div>
                        {restorable ? (
                          <button
                            type="button"
                            onClick={() => onRestoreEntry(entry.id)}
                            disabled={undoActionBusy || streaming}
                            className="flex-shrink-0 px-2 py-0.5 rounded bg-[var(--color-surface)] border border-[var(--color-border)] hover:border-[var(--color-accent)] text-xs text-[var(--color-text)] disabled:opacity-50"
                          >
                            Restore
                          </button>
                        ) : (
                          <span className="text-[10px] text-[var(--color-text2)]">Not restorable</span>
                        )}
                      </div>
                    );
                  })}
                </div>
              </div>
            )}

            {/* Accumulated Turn Diffs */}
            {diffRecords.length > 0 && (
              <div className="space-y-1.5 pt-2 border-t border-[var(--color-border)]">
                <span className="text-[10px] uppercase font-semibold text-[var(--color-text2)]">Turn Diffs</span>
                <div className="space-y-1.5">
                  {diffRecords.map((record, index) => {
                    const key = `${record.turn}-${record.file}-${index}`;
                    const expanded = expandedDiffs[key] ?? false;
                    return (
                      <div key={key} className="border border-[var(--color-border)] rounded bg-[var(--color-bg)] overflow-hidden">
                        <button
                          type="button"
                          onClick={() => toggleDiff(key)}
                          aria-expanded={Boolean(expandedDiffs[key])}
                          className="w-full px-2.5 py-1.5 text-left flex items-center justify-between hover:bg-[var(--color-surface2)] transition-colors"
                        >
                          <span className="truncate font-mono text-[11px]">
                            turn {record.turn} · {record.file}
                          </span>
                          {expanded ? <ChevronDown className="w-3.5 h-3.5" /> : <ChevronRight className="w-3.5 h-3.5" />}
                        </button>
                        {expanded && (
                          <pre className="p-2 border-t border-[var(--color-border)] font-mono text-[10px] leading-relaxed whitespace-pre-wrap break-all text-[var(--color-text)] max-h-52 overflow-auto bg-[var(--color-surface)]">
                            {record.diff}
                          </pre>
                        )}
                      </div>
                    );
                  })}
                </div>
              </div>
            )}
          </div>
        )}

        {/* ── Tab 2: Workflow ── */}
        {tab === 'workflow' && (
          <div className="space-y-4">
            {/* Notices */}
            {sessionNotice && (
              <div className="p-2.5 rounded bg-[var(--color-warning)]/10 border border-[var(--color-warning)]/40 text-[11px] text-[var(--color-warning)] flex items-start gap-1.5">
                <AlertTriangle className="w-3.5 h-3.5 flex-shrink-0 mt-0.5" />
                <span>{sessionNotice}</span>
              </div>
            )}
            {workstreamNotice && (
              <div className="p-2 rounded bg-[var(--color-surface2)] border border-[var(--color-border)] text-[11px] text-[var(--color-text2)]">
                {workstreamNotice}
              </div>
            )}

            {/* Workstream Section */}
            <div className="space-y-1.5">
              <span className="text-[10px] uppercase font-semibold text-[var(--color-text2)]">Workstream</span>
              <div className="flex items-center gap-1.5">
                <select
                  value={selectedWorkstreamId}
                  onChange={(e) => onSelectWorkstream(e.target.value)}
                  disabled={planActionBusy || streaming}
                  className="flex-1 bg-[var(--color-bg)] border border-[var(--color-border)] rounded px-2 py-1.5 text-xs text-[var(--color-text)] focus:outline-none focus:border-[var(--color-accent)]"
                >
                  <option value="" disabled={cannotClearWorkstream}>No workstream</option>
                  {workstreams.map((ws) => (
                    <option key={ws.id} value={ws.id}>{ws.title}</option>
                  ))}
                </select>
                <button
                  type="button"
                  onClick={onCreateWorkstream}
                  disabled={planActionBusy || streaming}
                  className="flex items-center gap-1 px-2.5 py-1.5 rounded border border-[var(--color-border)] hover:border-[var(--color-accent)] text-xs text-[var(--color-text)]"
                  title="Create new workstream"
                >
                  <Plus className="w-3.5 h-3.5" />
                  <span>New</span>
                </button>
                <button
                  type="button"
                  onClick={onExportHandoff}
                  disabled={!selectedWorkstreamId}
                  className="px-2.5 py-1.5 rounded border border-[var(--color-border)] hover:border-[var(--color-accent)] text-xs text-[var(--color-text)] disabled:opacity-40"
                  title="Export handoff markdown"
                >
                  Handoff
                </button>
              </div>
            </div>

            {/* Plan Section */}
            {selectedWorkstreamId && (
              <div className="space-y-3 pt-3 border-t border-[var(--color-border)]">
                <div className="space-y-1.5">
                  <span className="text-[10px] uppercase font-semibold text-[var(--color-text2)]">Plan</span>
                  <select
                    value={selectedPlanId}
                    onChange={(e) => onSelectPlan(e.target.value)}
                    disabled={planActionBusy || plansLoading || streaming}
                    className="w-full bg-[var(--color-bg)] border border-[var(--color-border)] rounded px-2 py-1.5 text-xs text-[var(--color-text)] focus:outline-none focus:border-[var(--color-accent)]"
                  >
                    <option value="" disabled={cannotClearPlan}>Chat only (no plan)</option>
                    {plans.map((p) => (
                      <option key={p.id} value={p.id}>
                        {p.definition.name || p.definition.objective || p.id} · {p.status} · r{p.revision}
                      </option>
                    ))}
                  </select>
                </div>

                {selectedPlan && (
                  <div className="space-y-2.5 p-2.5 rounded border border-[var(--color-border)] bg-[var(--color-bg)]">
                    {/* Status & Revision summary */}
                    <div className="flex items-center justify-between text-[11px]">
                      <span
                        className={`font-semibold ${
                          selectedPlan.approvedRevision === selectedPlan.revision
                            ? 'text-[var(--color-green)]'
                            : 'text-[var(--color-warning)]'
                        }`}
                      >
                        {selectedPlan.approvedRevision === selectedPlan.revision
                          ? `승인됨 · r${selectedPlan.revision}`
                          : `승인 필요 · ${selectedPlan.status} · r${selectedPlan.revision}`}
                      </span>
                      <span className="text-[var(--color-text2)] font-mono">
                        {selectedPlan.stages.filter((s) => s.status === 'completed').length}/{selectedPlan.stages.length} stages
                      </span>
                    </div>

                    {/* Action buttons */}
                    <div className="flex flex-wrap gap-1.5 pt-1">
                      {selectedPlanRevision !== selectedPlan.revision && (
                        <button
                          type="button"
                          onClick={onBindCurrentPlanRevision}
                          disabled={planActionBusy || streaming}
                          className="flex-1 px-2 py-1 rounded border border-[var(--color-border)] hover:border-[var(--color-accent)] text-xs text-[var(--color-text)]"
                        >
                          현재 revision 연결
                        </button>
                      )}

                      <button
                        type="button"
                        onClick={onApproveSelectedPlan}
                        disabled={
                          planActionBusy ||
                          streaming ||
                          selectedPlan.status !== 'draft' ||
                          selectedPlanRevision !== selectedPlan.revision ||
                          !selectedPlan.revision ||
                          !selectedPlan.stateRevision
                        }
                        className="flex-1 px-2.5 py-1 rounded bg-[var(--color-accent)] text-[var(--color-bg)] hover:opacity-85 text-xs disabled:opacity-40"
                        title={
                          selectedPlan.status !== 'draft'
                            ? 'Draft Plan만 승인할 수 있습니다.'
                            : '현재 definition/state revision을 CAS로 승인합니다.'
                        }
                      >
                        {planActionBusy ? '승인 중…' : 'Plan 승인'}
                      </button>
                    </div>

                    {/* Stage selector */}
                    <div className="space-y-1 pt-2">
                      <div className="flex items-center justify-between text-[10px] text-[var(--color-text2)]">
                        <span className="uppercase">Selected Stage</span>
                        {selectedStage && (
                          <span className="font-mono text-[var(--color-accent)]">{selectedStage.status}</span>
                        )}
                      </div>
                      <select
                        value={selectedStageId}
                        onChange={(e) => onSelectStage(e.target.value)}
                        disabled={planActionBusy || streaming || selectedPlan.stages.length === 0}
                        className="w-full bg-[var(--color-surface)] border border-[var(--color-border)] rounded px-2 py-1 text-xs text-[var(--color-text)] focus:outline-none"
                      >
                        {!selectedStageId && <option value="">Select stage</option>}
                        {selectedPlan.stages.map((stage) => {
                          const eligible = isStageEligible(selectedPlan, stage.id);
                          const name = selectedPlan.definition.stages?.find((item) => item.id === stage.id)?.name || stage.id;
                          return (
                            <option key={stage.id} value={stage.id} disabled={!eligible && stage.id !== selectedStageId}>
                              {name} · {stage.status} {!eligible && stage.id !== selectedStageId ? '(대기)' : ''}
                            </option>
                          );
                        })}
                      </select>
                    </div>

                    {/* Stage Reconcile (중단 단계 복구) */}
                    {isStageRunning && (
                      <div className="pt-2">
                        {!confirmReconcile ? (
                          <button
                            type="button"
                            onClick={() => setConfirmReconcile(true)}
                            disabled={planActionBusy || streaming}
                            className="w-full px-2 py-1.5 rounded border border-[var(--color-warning)] text-[var(--color-warning)] hover:bg-[var(--color-warning)]/10 text-xs font-medium flex items-center justify-center gap-1.5"
                          >
                            <AlertTriangle className="w-3.5 h-3.5" />
                            <span>중단 단계 복구</span>
                          </button>
                        ) : (
                          <div className="p-2 rounded border border-[var(--color-warning)] bg-[var(--color-warning)]/10 space-y-2">
                            <div className="text-[11px] text-[var(--color-text)] leading-relaxed">
                              아직 실행 중으로 기록된 단계 시도를 <strong>미완료</strong>로 표시해 다시 실행할 수 있게 합니다.
                              파일은 바꾸지 않습니다. 이전 프로세스가 멈췄는지, 남은 파일 변경을 확인한 뒤 진행하세요.
                            </div>
                            <div className="flex gap-1.5">
                              <button
                                type="button"
                                onClick={() => {
                                  setConfirmReconcile(false);
                                  onReconcileSelectedPlanStage();
                                }}
                                disabled={planActionBusy || streaming}
                                className="flex-1 px-2 py-1 rounded border border-[var(--color-warning)] text-[var(--color-text)] hover:bg-[var(--color-warning)]/10 text-xs font-medium disabled:opacity-50"
                              >
                                미완료로 표시
                              </button>
                              <button
                                type="button"
                                onClick={() => setConfirmReconcile(false)}
                                className="px-2 py-1 rounded border border-[var(--color-border)] hover:bg-[var(--color-surface)] text-xs text-[var(--color-text)]"
                              >
                                취소
                              </button>
                            </div>
                          </div>
                        )}
                      </div>
                    )}
                  </div>
                )}
              </div>
            )}

            {/* Session Info */}
            <div className="pt-3 border-t border-[var(--color-border)] space-y-1 text-[11px] text-[var(--color-text2)]">
              <div>
                <span className="text-[var(--color-text)] font-medium">Session: </span>
                <span className="font-mono">
                  {currentSession ? `${currentSession.id.slice(0, 8)} · r${currentSession.revision}` : 'New session'}
                </span>
              </div>
              {currentSession?.lifecycleStatus && (
                <div>
                  <span className="text-[var(--color-text)] font-medium">Lifecycle: </span>
                  <span className="font-mono">{currentSession.lifecycleStatus}</span>
                </div>
              )}
            </div>
          </div>
        )}

        {/* ── Tab 3: Receipt ── */}
        {tab === 'receipt' && (
          <div className="space-y-3">
            {latestReceipt && receiptVerdict ? (
              <div className="space-y-3">
                {/* Verdict banner: terminal state outranks the verification command */}
                <div className={`p-3 rounded-lg border flex items-center gap-2.5 ${receiptToneClass[receiptVerdict.tone]}`}>
                  {receiptVerdict.tone === 'success' ? (
                    <CheckCircle2 aria-hidden="true" className="w-5 h-5 flex-shrink-0" />
                  ) : receiptVerdict.tone === 'danger' ? (
                    <XCircle aria-hidden="true" className="w-5 h-5 flex-shrink-0" />
                  ) : receiptVerdict.tone === 'warning' ? (
                    <AlertTriangle aria-hidden="true" className="w-5 h-5 flex-shrink-0" />
                  ) : (
                    <HelpCircle aria-hidden="true" className="w-5 h-5 flex-shrink-0" />
                  )}
                  <div>
                    <div className="font-semibold text-xs">{receiptVerdict.label}</div>
                    {receiptVerdict.detail && (
                      <div className="text-[10px] text-[var(--color-text2)] font-mono">{receiptVerdict.detail}</div>
                    )}
                  </div>
                </div>

                {/* Receipt Details Card */}
                <div className="p-2.5 rounded border border-[var(--color-border)] bg-[var(--color-bg)] space-y-2">
                  <span className="text-[10px] uppercase font-semibold text-[var(--color-text2)]">Run Evidence Details</span>
                  <div className="space-y-1.5 text-[11px]">
                    <div className="flex justify-between">
                      <span className="text-[var(--color-text2)]">Verification command:</span>
                      <span className="font-mono text-[var(--color-text)]">{latestReceipt.verificationStatus || 'not-run'}</span>
                    </div>
                    {latestReceipt.terminalState && (
                      <div className="flex justify-between">
                        <span className="text-[var(--color-text2)]">Terminal State:</span>
                        <span className="font-mono text-[var(--color-text)]">{latestReceipt.terminalState}</span>
                      </div>
                    )}
                    {latestReceipt.completionStatus && (
                      <div className="flex justify-between">
                        <span className="text-[var(--color-text2)]">Completion:</span>
                        <span className="font-mono text-[var(--color-text)]">
                          {latestReceipt.completionStatus}
                          {latestReceipt.completionRevision != null ? ` (r${latestReceipt.completionRevision})` : ''}
                        </span>
                      </div>
                    )}
                    {latestReceipt.stopReason && (
                      <div className="flex justify-between">
                        <span className="text-[var(--color-text2)]">Stop Reason:</span>
                        <span className="font-mono text-[var(--color-text)]">{latestReceipt.stopReason}</span>
                      </div>
                    )}
                    {latestReceipt.timestamp && (
                      <div className="flex justify-between">
                        <span className="text-[var(--color-text2)]">Recorded At:</span>
                        <span className="text-[var(--color-text)]">{latestReceipt.timestamp.toLocaleTimeString()}</span>
                      </div>
                    )}
                    {latestReceipt.receiptPath && (
                      <div className="pt-1.5 border-t border-[var(--color-border)]">
                        <div className="text-[10px] text-[var(--color-text2)] mb-0.5">Receipt File:</div>
                        <div className="font-mono text-[10px] text-[var(--color-text)] truncate bg-[var(--color-surface)] p-1 rounded" title={latestReceipt.receiptPath}>
                          {latestReceipt.receiptPath}
                        </div>
                      </div>
                    )}
                  </div>
                </div>

                {/* Evidence records from the stored receipt */}
                {latestReceipt.receiptPath && (
                  <div className="p-2.5 rounded border border-[var(--color-border)] bg-[var(--color-bg)] space-y-2">
                    <span className="text-[10px] uppercase font-semibold text-[var(--color-text2)]">Evidence</span>
                    {receiptEvidence?.path !== latestReceipt.receiptPath ? (
                      <div className="text-[11px] text-[var(--color-text2)]">Loading evidence…</div>
                    ) : receiptEvidence.records.length === 0 ? (
                      <div className="text-[11px] text-[var(--color-text2)]">No evidence records were stored for this run.</div>
                    ) : (
                      <ul className="space-y-1.5">
                        {receiptEvidence.records.map((record, idx) => (
                          <li key={`${record.source}-${idx}`} className="text-[11px] space-y-0.5">
                            <div className="flex justify-between gap-2">
                              <span className="font-mono text-[var(--color-text)] truncate">{record.command || record.source || 'evidence'}</span>
                              <span className="font-mono text-[var(--color-text2)] shrink-0">
                                {record.status}{record.exitCode != null ? ` · exit ${record.exitCode}` : ''}
                              </span>
                            </div>
                            {record.summary && <div className="text-[var(--color-text2)] truncate">{record.summary}</div>}
                          </li>
                        ))}
                      </ul>
                    )}
                  </div>
                )}
              </div>
            ) : (
              <div className="py-8 text-center text-[var(--color-text2)] space-y-1.5">
                <ShieldCheck className="w-8 h-8 mx-auto text-[var(--color-text2)]/40" />
                <div className="font-medium text-xs">현재 턴의 검증 영수증이 없습니다</div>
                <div className="text-[11px]">작업 완료 후 검증 상태와 영수증이 여기에 기록됩니다.</div>
              </div>
            )}

            {/* History Link */}
            {onNavigateHistory && (
              <div className="pt-2 border-t border-[var(--color-border)]">
                <button
                  type="button"
                  onClick={() => onNavigateHistory('receipts')}
                  className="w-full flex items-center gap-1.5 text-xs text-[var(--color-accent)] hover:underline p-1.5 rounded hover:bg-[var(--color-surface2)] text-left"
                >
                  <ExternalLink aria-hidden="true" className="w-3.5 h-3.5" />
                  <span>최근 검증 영수증 목록 (History)</span>
                </button>
              </div>
            )}
          </div>
        )}
      </div>
    </aside>
  );
}
