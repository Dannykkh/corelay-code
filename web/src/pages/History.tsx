import { useState, useEffect, useRef } from 'react';
import { CostsPage } from './Costs';
import { KairosPage } from './Kairos';
import { fetchJSON } from '../lib/api';
import { ReceiptBadge } from '../components/ReceiptBadge';
import {
  Activity,
  Receipt,
  Bell,
  ExternalLink,
  RefreshCw,
  FolderGit2,
} from 'lucide-react';

export type HistoryTab = 'costs' | 'receipts' | 'notifications';

// Mirrors evidenceRecentItem in internal/server/evidence_api.go.
interface RecentReceiptItem {
  kind?: string;
  status?: string;
  terminalState?: string;
  gate?: string;
  source?: string;
  command?: string;
  summary?: string;
  createdAt?: string;
  receiptPath?: string;
  workDir?: string;
  evidenceCount?: number;
  /** Team receipts: number of failed tasks. */
  failed?: number;
}

interface EvidenceRecentResponse {
  baseDir: string;
  scope: string;
  workDir: string;
  items: RecentReceiptItem[];
}

const TABS: { id: HistoryTab; label: string; icon: typeof Activity }[] = [
  { id: 'costs', label: 'Activity & Costs', icon: Activity },
  { id: 'receipts', label: 'Verification Receipts', icon: Receipt },
  { id: 'notifications', label: 'Notifications & Logs', icon: Bell },
];

export function HistoryPage({ initialTab = 'costs' }: { initialTab?: HistoryTab }) {
  const [tab, setTab] = useState<HistoryTab>(initialTab);
  const [receipts, setReceipts] = useState<RecentReceiptItem[]>([]);
  const [receiptsLoading, setReceiptsLoading] = useState(false);
  const [receiptsError, setReceiptsError] = useState(false);
  const [selectedReceipt, setSelectedReceipt] = useState<RecentReceiptItem | null>(null);
  const closeButtonRef = useRef<HTMLButtonElement>(null);

  const loadReceipts = async () => {
    setReceiptsLoading(true);
    setReceiptsError(false);
    try {
      const data = await fetchJSON<EvidenceRecentResponse>('/api/evidence/recent?scope=all&limit=30');
      setReceipts(data?.items || []);
    } catch {
      setReceiptsError(true);
    } finally {
      setReceiptsLoading(false);
    }
  };

  useEffect(() => {
    if (tab === 'receipts') {
      void loadReceipts();
    }
  }, [tab]);

  useEffect(() => {
    if (selectedReceipt) closeButtonRef.current?.focus();
  }, [selectedReceipt]);

  return (
    <div className="flex flex-col h-full w-full bg-[var(--color-bg)]">
      {/* Top History Navigation */}
      <div className="px-6 py-2.5 border-b border-[var(--color-border)] bg-[var(--color-surface)] flex items-center justify-between">
        <div role="tablist" aria-label="History" className="flex items-center gap-1 bg-[var(--color-surface2)] p-1 rounded-lg">
          {TABS.map(({ id, label, icon: Icon }) => (
            <button
              key={id}
              id={`history-tab-${id}`}
              type="button"
              role="tab"
              aria-selected={tab === id}
              aria-controls="history-panel"
              onClick={() => setTab(id)}
              className={`flex items-center gap-1.5 px-3 py-1 rounded-md text-xs font-medium transition-colors ${
                tab === id
                  ? 'bg-[var(--color-surface)] text-[var(--color-text)] shadow-sm'
                  : 'text-[var(--color-text2)] hover:text-[var(--color-text)]'
              }`}
            >
              <Icon aria-hidden="true" className="w-3.5 h-3.5" />
              {label}
            </button>
          ))}
        </div>

        {tab === 'receipts' && (
          <button
            type="button"
            onClick={loadReceipts}
            disabled={receiptsLoading}
            className="flex items-center gap-1 px-2.5 py-1 text-xs text-[var(--color-text2)] hover:text-[var(--color-text)] rounded border border-[var(--color-border)] hover:bg-[var(--color-surface2)] transition-colors"
          >
            <RefreshCw aria-hidden="true" className={`w-3 h-3 ${receiptsLoading ? 'animate-spin' : ''}`} />
            Refresh
          </button>
        )}
      </div>

      {/* Main Tab Content */}
      <div id="history-panel" role="tabpanel" aria-labelledby={`history-tab-${tab}`} className="flex-1 overflow-y-auto">
        {tab === 'costs' && <CostsPage />}

        {tab === 'receipts' && (
          <div className="max-w-5xl mx-auto p-6 space-y-4">
            <div>
              <h2 className="text-base font-semibold text-[var(--color-text)]">Turn Verification Receipts</h2>
              <p className="text-xs text-[var(--color-text2)] mt-0.5">
                Recent run receipts: the verification command result and the evidence gate&apos;s terminal state.
              </p>
            </div>

            {receiptsLoading && receipts.length === 0 ? (
              <div className="py-12 text-center text-xs text-[var(--color-text2)]">
                Loading verification receipts…
              </div>
            ) : receiptsError && receipts.length === 0 ? (
              <div className="py-12 text-center text-xs text-[var(--color-red)] border border-dashed border-[var(--color-border)] rounded-xl">
                Could not load receipts. Refresh to retry.
              </div>
            ) : receipts.length === 0 ? (
              <div className="py-12 text-center text-xs text-[var(--color-text2)] border border-dashed border-[var(--color-border)] rounded-xl">
                No verification receipts recorded yet.
              </div>
            ) : (
              <div className="border border-[var(--color-border)] rounded-xl overflow-hidden bg-[var(--color-surface)] divide-y divide-[var(--color-border)]">
                {receipts.map((rcpt, idx) => (
                  <button
                    type="button"
                    key={rcpt.receiptPath || idx}
                    onClick={() => setSelectedReceipt(rcpt)}
                    className="w-full text-left p-3.5 hover:bg-[var(--color-surface2)]/60 cursor-pointer transition-colors flex items-center justify-between gap-4"
                  >
                    <div className="min-w-0">
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="font-semibold text-xs text-[var(--color-text)]">{rcpt.kind || 'agent-run'}</span>
                        <ReceiptBadge
                          receipt={{
                            terminalState: rcpt.terminalState || ((rcpt.failed ?? 0) > 0 ? 'failed' : undefined),
                            verificationStatus: rcpt.status,
                          }}
                        />
                        {rcpt.gate && (
                          <span className="text-[10px] font-mono px-1.5 py-0.5 bg-[var(--color-surface2)] text-[var(--color-text2)] rounded">
                            gate: {rcpt.gate}
                          </span>
                        )}
                      </div>
                      {rcpt.workDir && (
                        <div className="flex items-center gap-1 text-[11px] text-[var(--color-text2)] mt-0.5 truncate">
                          <FolderGit2 aria-hidden="true" className="w-3 h-3 shrink-0" />
                          <span className="truncate">{rcpt.workDir}</span>
                        </div>
                      )}
                    </div>

                    <div className="flex items-center gap-3 shrink-0 text-right">
                      {rcpt.createdAt && (
                        <span className="text-[11px] text-[var(--color-text2)]">
                          {new Date(rcpt.createdAt).toLocaleString()}
                        </span>
                      )}
                      <ExternalLink aria-hidden="true" className="w-3.5 h-3.5 text-[var(--color-text2)]" />
                    </div>
                  </button>
                ))}
              </div>
            )}

            {/* Receipt Detail Modal */}
            {selectedReceipt && (
              <div
                role="dialog"
                aria-modal="true"
                aria-labelledby="receipt-modal-title"
                className="fixed inset-0 z-50 bg-[var(--color-bg)]/80 flex items-center justify-center p-4"
                onKeyDown={(e) => {
                  if (e.key === 'Escape') setSelectedReceipt(null);
                }}
              >
                <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl max-w-2xl w-full max-h-[85vh] flex flex-col shadow-2xl">
                  <div className="px-5 py-3 border-b border-[var(--color-border)] flex items-center justify-between">
                    <div className="flex items-center gap-2">
                      <Receipt aria-hidden="true" className="w-4 h-4 text-[var(--color-accent)]" />
                      <span id="receipt-modal-title" className="font-semibold text-sm text-[var(--color-text)]">
                        Receipt Details
                      </span>
                    </div>
                    <button
                      ref={closeButtonRef}
                      type="button"
                      onClick={() => setSelectedReceipt(null)}
                      className="text-xs text-[var(--color-text2)] hover:text-[var(--color-text)]"
                    >
                      Close
                    </button>
                  </div>
                  <div className="p-5 overflow-y-auto space-y-3 font-mono text-xs">
                    <pre className="p-3 bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg whitespace-pre-wrap break-all text-[var(--color-text)]">
                      {JSON.stringify(selectedReceipt, null, 2)}
                    </pre>
                  </div>
                </div>
              </div>
            )}
          </div>
        )}

        {tab === 'notifications' && <KairosPage />}
      </div>
    </div>
  );
}
