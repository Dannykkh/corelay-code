import { useState, useRef, useEffect, useCallback } from 'react';
import { t, getLang } from '../lib/i18n';
import { CircleX, LoaderCircle, ShieldAlert, Mic, Paperclip, Volume2, X, FolderGit2, GitBranch, SlidersHorizontal, RotateCcw, ExternalLink, Compass, Layers, Bug, CodeXml } from 'lucide-react';
import { Markdown } from '../components/Markdown';
import { HTTPError, resolveApproval, fetchJSON, type ApprovalDecision } from '../lib/api';
import { Inspector, type InspectorTab, type RunReceipt } from '../components/Inspector';
import { StepCard } from '../components/StepCard';
import { ReceiptBadge } from '../components/ReceiptBadge';
import type { HistoryTab } from './History';
import { loadPreferredExecutionMode } from '../lib/executionMode';
import { WorkspaceModeSwitch, type WorkspaceMode } from '../components/WorkspaceModeSwitch';
import { streamSSE } from '../lib/sse';
import {
  getSession,
  listSessions,
  saveSession,
  SessionConflictError,
  type SessionExecutionMode,
  type SessionExecutionPolicy,
  type SessionLifecycleStatus,
  type SessionImageReference,
  type SessionMessage,
  type SessionSummary,
} from '../lib/sessions';
import {
  approveWorkstreamPlan,
  createWorkstream,
  generateHandoff,
  getWorkstreamPlan,
  listWorkstreamPlans,
  listWorkstreams,
  reconcileWorkstreamPlanStage,
  type Workstream,
  type WorkstreamPlan,
} from '../lib/workstreams';

interface ChatMessage {
  role: 'user' | 'assistant' | 'tool';
  content: string;
  attachments?: SessionImageReference[];
  toolName?: string;
  toolCallId?: string;
  toolInput?: Record<string, unknown> | string;
  toolResult?: string;
  toolDiff?: string;
  isError?: boolean;
  timestamp: Date;
  receipt?: RunReceipt;
}

// Tool events carry the provider call id. With an id only the card of that exact
// call matches (a reloaded card without an id never absorbs another call); name
// matching is only for events from servers that did not send an id.
function findOpenToolMessage(messages: ChatMessage[], toolCallId?: string, toolName?: string): number {
  for (let i = messages.length - 1; i >= 0; i--) {
    const message = messages[i];
    if (message.role !== 'tool' || message.toolResult !== undefined) continue;
    if (toolCallId ? message.toolCallId === toolCallId : message.toolName === toolName) return i;
  }
  return -1;
}

interface DiffRecord {
  turn: number;
  file: string;
  diff: string;
}

interface UndoEntry {
  id: string;
  path: string;
  status: string;
}

interface AttachedImage {
  mediaType: string;
  data: string;
}

type AgentContentBlock =
  | { type: 'text'; text: string }
  | { type: 'image'; source: { type: 'base64'; media_type: string; data: string } };

type AgentMessage = {
  role: 'user' | 'assistant';
  content: string | AgentContentBlock[];
};

const maxAttachedImageBytes = 4 * 1024 * 1024;
const supportedImageTypes = new Set(['image/png', 'image/jpeg', 'image/gif', 'image/webp']);

function readImageAttachment(file: File): Promise<AttachedImage> {
  const mediaType = file.type.toLowerCase();
  if (!supportedImageTypes.has(mediaType)) {
    return Promise.reject(new Error('PNG, JPEG, GIF, and WebP images are supported.'));
  }
  if (file.size === 0 || file.size > maxAttachedImageBytes) {
    return Promise.reject(new Error('Image must be between 1 byte and 4 MiB.'));
  }

  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(new Error('Image could not be read.'));
    reader.onload = () => {
      if (typeof reader.result !== 'string') {
        reject(new Error('Image could not be read.'));
        return;
      }
      const separator = reader.result.indexOf(',');
      if (separator < 0) {
        reject(new Error('Image data URL is invalid.'));
        return;
      }
      resolve({ mediaType, data: reader.result.slice(separator + 1) });
    };
    reader.readAsDataURL(file);
  });
}

interface ChatPageProps {
  selectedWorkspace: string;
  loadSessionId?: string | null;
  onSessionLoaded?: () => void;
  onOpenHistory?: (tab?: HistoryTab) => void;
  onApprovalPendingChange?: (pending: boolean) => void;
  onWorkspaceModeChange?: (mode: WorkspaceMode) => void;
  workspaceModeAttention?: Partial<Record<WorkspaceMode, boolean>>;
}

type AgentEvent = {
  type: string;
  data?: unknown;
};

type AgentEventObject = {
  name?: string;
  input?: Record<string, unknown> | string;
  result?: string;
  isError?: boolean;
  file?: string;
  diff?: string;
  entries?: unknown;
  error?: string;
  chars?: number;
  elapsedMs?: number;
  id?: string;
  planMode?: boolean;
  sessionId?: string;
  durableSessionId?: string;
  durableRevision?: number;
  revision?: number;
  lifecycleStatus?: string;
  reconcileRequired?: boolean;
  code?: string;
  message?: string;
  toolName?: string;
  toolCallId?: string;
  redactedInput?: string;
  dangerLevel?: string;
  scope?: string;
  expiresAt?: string;
  terminalState?: string;
  completionStatus?: string;
  completionRevision?: number;
  completionBlocked?: number;
  executionPolicy?: unknown;
};

type ApprovalState = 'pending' | 'submitting' | 'resolved' | 'expired' | 'error';

type ActiveApproval = {
  id: string;
  runtimeSessionId: string;
  toolName: string;
  toolCallId?: string;
  redactedInput: string;
  dangerLevel?: string;
  scope?: string;
  expiresAt?: string;
  state: ApprovalState;
  decision?: ApprovalDecision;
  error?: string;
};

type WorkflowBinding = {
  workstreamId: string;
  planId: string;
  planRevision?: number;
  stageId: string;
};

type CurrentSessionIndicator = {
  id: string;
  title: string;
  revision: number;
  lifecycleStatus: SessionLifecycleStatus;
};

function readExecutionPolicy(value: unknown): SessionExecutionPolicy | null {
  if (!value || typeof value !== 'object') return null;
  const policy = value as Partial<SessionExecutionPolicy>;
  if (policy.mode !== 'read-only' && policy.mode !== 'workspace' && policy.mode !== 'full') {
    return null;
  }
  if (typeof policy.revision !== 'number' || !Number.isSafeInteger(policy.revision) || policy.revision < 1) {
    return null;
  }
  return { mode: policy.mode, revision: policy.revision };
}

function readUndoEntries(value: unknown): UndoEntry[] {
  if (!Array.isArray(value)) return [];
  return value.flatMap((item): UndoEntry[] => {
    if (!item || typeof item !== 'object') return [];
    const entry = item as Record<string, unknown>;
    return typeof entry.id === 'string' && typeof entry.path === 'string' && typeof entry.status === 'string'
      ? [{ id: entry.id, path: entry.path, status: entry.status }]
      : [];
  });
}

function executionModeLabel(mode: SessionExecutionMode): string {
  switch (mode) {
    case 'read-only': return 'read-only';
    case 'workspace': return 'workspace';
    case 'full': return 'full';
  }
}

function workspaceLabel(workspace: string): string {
  const segments = workspace.replace(/\\/g, '/').split('/').filter(Boolean);
  return segments[segments.length - 1] || workspace || 'none';
}

type SpeechRecognitionLike = {
  continuous: boolean;
  interimResults: boolean;
  lang: string;
  onstart: (() => void) | null;
  onend: (() => void) | null;
  onresult: ((event: SpeechRecognitionEventLike) => void) | null;
  start: () => void;
  stop: () => void;
};

type SpeechRecognitionConstructor = new () => SpeechRecognitionLike;

type SpeechRecognitionEventLike = {
  results: ArrayLike<ArrayLike<{ transcript: string }>>;
};

function eventObject(data: unknown): AgentEventObject {
  return data && typeof data === 'object' ? data as AgentEventObject : {};
}

function completionBlocksSuccess(data: AgentEventObject): boolean {
  return data.terminalState === 'blocked' ||
    data.completionStatus === 'incomplete' ||
    data.completionStatus === 'blocked' ||
    (typeof data.completionBlocked === 'number' && data.completionBlocked > 0);
}

function eventText(data: unknown): string {
  if (typeof data === 'string') return data;
  if (data == null) return '';
  return JSON.stringify(data);
}

function planStageEligible(plan: WorkstreamPlan, stageId: string): boolean {
  const stageIndex = plan.stages.findIndex((stage) => stage.id === stageId);
  if (stageIndex < 0 || plan.stages.some((stage) => stage.status === 'running')) return false;
  const stage = plan.stages[stageIndex];
  if (stage.status !== 'pending' && stage.status !== 'failed') return false;
  return plan.stages.slice(0, stageIndex).every((previous) => previous.status === 'completed');
}

function planIsApprovedCurrentRevision(plan: WorkstreamPlan, revision: number | undefined): boolean {
  return revision != null && plan.revision === revision && plan.approvedRevision === plan.revision &&
    (plan.status === 'approved' || plan.status === 'executing' || plan.status === 'failed');
}

export function ChatPage({
  selectedWorkspace,
  loadSessionId,
  onSessionLoaded,
  onOpenHistory,
  onApprovalPendingChange,
  onWorkspaceModeChange,
  workspaceModeAttention,
}: ChatPageProps) {
  const ko = getLang() === 'ko';
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState('');
  const [streaming, setStreaming] = useState(false);
  const [status, setStatus] = useState('');
  const [planReady, setPlanReady] = useState(false); // a /plan run finished; offer an ordinary follow-up prompt
  const [attachedImage, setAttachedImage] = useState<AttachedImage | null>(null);
  const [isListening, setIsListening] = useState(false);
  const [, setSessions] = useState<SessionSummary[]>([]);
  const [workstreams, setWorkstreams] = useState<Workstream[]>([]);
  const [plans, setPlans] = useState<WorkstreamPlan[]>([]);
  const [plansLoading, setPlansLoading] = useState(false);
  const [planActionBusy, setPlanActionBusy] = useState(false);
  const [activeWorkspace, setActiveWorkspace] = useState(selectedWorkspace);
  const [selectedWorkstreamId, setSelectedWorkstreamId] = useState('');
  const [selectedPlanId, setSelectedPlanId] = useState('');
  const [selectedPlanRevision, setSelectedPlanRevision] = useState<number | undefined>();
  const [selectedStageId, setSelectedStageId] = useState('');
  const [workstreamNotice, setWorkstreamNotice] = useState('');
  const [sessionNotice, setSessionNotice] = useState('');
  const [activeApproval, setActiveApproval] = useState<ActiveApproval | null>(null);
  const [currentSession, setCurrentSession] = useState<CurrentSessionIndicator | null>(null);
  const [effectiveExecutionPolicy, setEffectiveExecutionPolicy] = useState<SessionExecutionPolicy | null>(null);
  const [executionPolicyPending, setExecutionPolicyPending] = useState(false);
  // Liveness indicators for slow local models: elapsed seconds tick client-side
  // from the moment we send; genChars is the authoritative output size from the
  // backend heartbeat. Together they prove the model is alive, not hung —
  // modeled on Claude Code's "Thinking… (Ns · ↑N)" status line.
  const [elapsed, setElapsed] = useState(0);
  const [genChars, setGenChars] = useState(0);
  const [diffRecords, setDiffRecords] = useState<DiffRecord[]>([]);
  const [undoEntries, setUndoEntries] = useState<UndoEntry[]>([]);
  const [undoActionBusy, setUndoActionBusy] = useState(false);
  const [inspectorOpen, setInspectorOpen] = useState(false);
  const [inspectorTab, setInspectorTab] = useState<InspectorTab>('changes');
  const [latestReceipt, setLatestReceipt] = useState<RunReceipt | null>(null);
  const [gitBranch, setGitBranch] = useState<string | null>(null);
  const tickRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  const durableSessionIDRef = useRef<string | null>(null);
  const durableSessionRevisionRef = useRef<number | null>(null);
  const durableSessionEpochRef = useRef(0);
  const durableSessionSaveChainRef = useRef<Promise<void>>(Promise.resolve());
  const persistedWorkflowBindingRef = useRef<WorkflowBinding>({ workstreamId: '', planId: '', stageId: '' });
  const plansRequestRef = useRef(0);
  const workstreamsRequestRef = useRef(0);
  const workspaceEpochRef = useRef(0);
  const sendInProgressRef = useRef(false);
  const turnNumberRef = useRef(0);
  const activeWorkspaceRef = useRef(selectedWorkspace);
  const runtimeSessionIdRef = useRef<string | null>(null);
  const bottomRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const denyApprovalRef = useRef<HTMLButtonElement>(null);

  const updateActiveWorkspace = useCallback((workspace: string) => {
    if (activeWorkspaceRef.current !== workspace) {
      workspaceEpochRef.current += 1;
      abortRef.current?.abort();
      runtimeSessionIdRef.current = null;
    }
    activeWorkspaceRef.current = workspace;
    setActiveWorkspace(workspace);
  }, []);

  // A selected project is the default for a new chat. Once a durable session
  // is loaded, its stored workspace remains authoritative across project
  // selection changes in another part of the UI.
  useEffect(() => {
    if (!durableSessionIDRef.current) {
      updateActiveWorkspace(selectedWorkspace);
    } else if (selectedWorkspace !== activeWorkspaceRef.current) {
      workspaceEpochRef.current += 1;
      abortRef.current?.abort();
      runtimeSessionIdRef.current = null;
    }
  }, [selectedWorkspace, updateActiveWorkspace]);

  useEffect(() => {
    let active = true;
    queueMicrotask(() => {
      if (!active) return;
      if (!activeWorkspace) {
        setSessions([]);
        return;
      }
      listSessions(activeWorkspace).then((sessions) => {
        if (active) setSessions(sessions || []);
      }).catch(() => {
        if (active) setSessions([]);
      });
    });
    return () => { active = false; };
  }, [activeWorkspace]);

  useEffect(() => {
    let active = true;
    const query = activeWorkspace ? `?workDir=${encodeURIComponent(activeWorkspace)}` : '';
    fetchJSON<{ branch?: string }>(`/api/kairos/git${query}`)
      .then((data) => {
        if (active && data?.branch) setGitBranch(data.branch);
        else if (active) setGitBranch(null);
      })
      .catch(() => {
        if (active) setGitBranch(null);
      });
    return () => { active = false; };
  }, [activeWorkspace]);

  const refreshWorkstreams = useCallback(async (workspace = activeWorkspaceRef.current) => {
    const requestID = ++workstreamsRequestRef.current;
    try {
      const next = await listWorkstreams(workspace);
      if (workstreamsRequestRef.current !== requestID || activeWorkspaceRef.current !== workspace) return;
      setWorkstreams(next);
      setSelectedWorkstreamId((current) => {
        if (!current) return current;
        return next.some((w) => w.id === current) ? current : '';
      });
    } catch (err) {
      if (workstreamsRequestRef.current === requestID && activeWorkspaceRef.current === workspace) {
        setWorkstreamNotice(err instanceof Error ? err.message : String(err));
      }
    }
  }, []);

  useEffect(() => {
    refreshWorkstreams(activeWorkspace);
  }, [activeWorkspace, refreshWorkstreams]);

  const refreshPlans = useCallback(async (workstreamId = selectedWorkstreamId, workspace = activeWorkspaceRef.current) => {
    const requestID = ++plansRequestRef.current;
    if (!workstreamId) {
      setPlans([]);
      setPlansLoading(false);
      return [];
    }
    setPlansLoading(true);
    try {
      const next = await listWorkstreamPlans(workstreamId, workspace);
      if (plansRequestRef.current === requestID) setPlans(next);
      return next;
    } catch (err) {
      if (plansRequestRef.current === requestID) {
        setPlans([]);
        setWorkstreamNotice(err instanceof Error ? err.message : String(err));
      }
      return [];
    } finally {
      if (plansRequestRef.current === requestID) setPlansLoading(false);
    }
  }, [selectedWorkstreamId]);

  useEffect(() => {
    void refreshPlans(selectedWorkstreamId, activeWorkspace);
  }, [activeWorkspace, selectedWorkstreamId, refreshPlans]);

  const loadSession = useCallback(async (id: string) => {
    abortRef.current?.abort();
    runtimeSessionIdRef.current = null;
    setActiveApproval(null);
    const epoch = durableSessionEpochRef.current + 1;
    durableSessionEpochRef.current = epoch;
    durableSessionSaveChainRef.current = Promise.resolve();
    const sess = await getSession(id);
    if (durableSessionEpochRef.current !== epoch) return;
    const msgs: ChatMessage[] = (sess.messages || []).map((m) => ({
      ...m,
      timestamp: new Date(m.timestamp),
    }));
    setMessages(msgs);
    durableSessionIDRef.current = sess.id;
    durableSessionRevisionRef.current = sess.revision ?? 0;
    setCurrentSession({
      id: sess.id,
      title: sess.title,
      revision: sess.revision ?? 0,
      lifecycleStatus: sess.lifecycleStatus ?? (sess.reconcileRequired ? 'recovery-needed' : 'active'),
    });
    setEffectiveExecutionPolicy(readExecutionPolicy(sess.executionPolicy));
    setExecutionPolicyPending(false);
    updateActiveWorkspace(sess.workspace || selectedWorkspace);
    setSelectedWorkstreamId(sess.workstreamId || '');
    const binding: WorkflowBinding = {
      workstreamId: sess.workstreamId || '',
      planId: sess.planId || '',
      planRevision: sess.planRevision,
      stageId: sess.stageId || '',
    };
    persistedWorkflowBindingRef.current = binding;
    setSelectedPlanId(binding.planId);
    setSelectedPlanRevision(binding.planRevision);
    setSelectedStageId(binding.stageId);
    setSessionNotice('');
    setDiffRecords([]);
    setUndoEntries([]);
    setLatestReceipt(null);
    turnNumberRef.current = 0;
  }, [selectedWorkspace, updateActiveWorkspace]);

  const newChat = useCallback(() => {
    abortRef.current?.abort();
    runtimeSessionIdRef.current = null;
    setActiveApproval(null);
    durableSessionEpochRef.current += 1;
    durableSessionSaveChainRef.current = Promise.resolve();
    setMessages([]);
    durableSessionIDRef.current = null;
    durableSessionRevisionRef.current = null;
    setCurrentSession(null);
    setEffectiveExecutionPolicy(null);
    setExecutionPolicyPending(false);
    persistedWorkflowBindingRef.current = { workstreamId: '', planId: '', stageId: '' };
    updateActiveWorkspace(selectedWorkspace);
    setSelectedWorkstreamId('');
    setSelectedPlanId('');
    setSelectedPlanRevision(undefined);
    setSelectedStageId('');
    setPlans([]);
    setSessionNotice('');
    setDiffRecords([]);
    setUndoEntries([]);
    setLatestReceipt(null);
    turnNumberRef.current = 0;
  }, [selectedWorkspace, updateActiveWorkspace]);

  // Handle session load/new from SidePanel
  useEffect(() => {
    if (!loadSessionId) return;
    if (loadSessionId === '__new__') {
      newChat();
    } else {
      loadSession(loadSessionId);
    }
    onSessionLoaded?.();
  }, [loadSessionId, onSessionLoaded, loadSession, newChat]);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, [messages, status, activeApproval]);

  // Stop the active stream and liveness clock if the component unmounts.
  useEffect(() => () => {
    abortRef.current?.abort();
    durableSessionEpochRef.current += 1;
    if (tickRef.current) clearInterval(tickRef.current);
  }, []);

  useEffect(() => {
    if (activeApproval?.state === 'pending') {
      denyApprovalRef.current?.focus();
    }
  }, [activeApproval?.id, activeApproval?.state]);

  useEffect(() => {
    if (!activeApproval?.expiresAt || activeApproval.state === 'resolved' || activeApproval.state === 'expired') {
      return;
    }
    const expiresAt = Date.parse(activeApproval.expiresAt);
    if (!Number.isFinite(expiresAt)) return;
    const expire = () => setActiveApproval((current) => (
      current?.id === activeApproval.id && current.state !== 'resolved'
        ? { ...current, state: 'expired', error: 'This approval request expired. No action was allowed.' }
        : current
    ));
    const delay = expiresAt - Date.now();
    if (delay <= 0) {
      expire();
      return;
    }
    const timer = window.setTimeout(expire, delay);
    return () => window.clearTimeout(timer);
  }, [activeApproval?.expiresAt, activeApproval?.id, activeApproval?.state]);

  // Persist one exact transcript revision before binding it to an agent run.
  // The returned promise belongs to this save; the internal tail swallows the
  // rejection only so a later save can still proceed after the caller handles
  // the conflict.
  const persistSession = useCallback((msgs: ChatMessage[], bindingOverride?: WorkflowBinding): Promise<void> => {
    if (msgs.length === 0) return Promise.resolve();
    const binding = bindingOverride || {
      workstreamId: selectedWorkstreamId,
      planId: selectedPlanId,
      planRevision: selectedPlanRevision,
      stageId: selectedStageId,
    };
    const sessionMsgs: SessionMessage[] = msgs.map((m) => ({
      role: m.role,
      content: m.content,
      attachments: m.attachments,
      toolName: m.toolName,
      toolInput: m.toolInput,
      toolResult: m.toolResult,
      isError: m.isError,
      timestamp: m.timestamp.toISOString(),
    }));
    const epoch = durableSessionEpochRef.current;
    const workspace = activeWorkspaceRef.current;
    const workspaceEpoch = workspaceEpochRef.current;

    // Serialize saves for one durable chat so each request observes the
    // revision returned by the previous request. A load/new-chat transition
    // advances the epoch; late responses from the old chat are then ignored
    // instead of rebinding the new chat to a stale session ID.
    const operation = durableSessionSaveChainRef.current
      .catch(() => undefined)
      .then(async () => {
        if (durableSessionEpochRef.current !== epoch
          || workspaceEpochRef.current !== workspaceEpoch
          || activeWorkspaceRef.current !== workspace) return;
        const sid = durableSessionIDRef.current;
        const revision = durableSessionRevisionRef.current;
        try {
          const result = await saveSession(
            {
              id: sid || undefined,
              messages: sessionMsgs,
              workspace: sid ? undefined : workspace || selectedWorkspace,
              workstreamId: binding.workstreamId || undefined,
              planId: binding.planId || undefined,
              planRevision: binding.planId ? binding.planRevision : undefined,
              stageId: binding.planId ? binding.stageId || undefined : undefined,
            },
            sid ? revision ?? undefined : undefined,
          );
          if (durableSessionEpochRef.current !== epoch
            || workspaceEpochRef.current !== workspaceEpoch
            || activeWorkspaceRef.current !== workspace) return;
          durableSessionIDRef.current = result.id;
          durableSessionRevisionRef.current = result.revision;
          setCurrentSession((current) => ({
            id: result.id,
            title: current?.id === result.id ? current.title : (msgs.find((message) => message.role === 'user')?.content || 'New session'),
            revision: result.revision,
            lifecycleStatus: current?.id === result.id ? current.lifecycleStatus : 'active',
          }));
          persistedWorkflowBindingRef.current = binding;
          setSessionNotice('');
          listSessions(activeWorkspaceRef.current).then((sessions) => {
            if (durableSessionEpochRef.current === epoch) setSessions(sessions || []);
          }).catch(() => {
            if (durableSessionEpochRef.current === epoch) setSessions([]);
          });
        } catch (error) {
          if (durableSessionEpochRef.current !== epoch) return;
          if (error instanceof SessionConflictError) {
            setSessionNotice(`Session save conflict: ${error.message}. Reload this session before saving again.`);
          } else {
            setSessionNotice(`Session save failed: ${error instanceof Error ? error.message : String(error)}`);
          }
          throw error;
        }
      }
    );
    durableSessionSaveChainRef.current = operation.catch(() => undefined);
    return operation;
  }, [selectedWorkspace, selectedWorkstreamId, selectedPlanId, selectedPlanRevision, selectedStageId]);

  async function send(overrideText?: string) {
    const text = (overrideText ?? input).trim();
    if (!text && !attachedImage) return;
    if (streaming || sendInProgressRef.current) return;
    sendInProgressRef.current = true;
    turnNumberRef.current += 1;
    const sendWorkspace = activeWorkspaceRef.current;
    const sendWorkspaceEpoch = workspaceEpochRef.current;
    const sendIsCurrent = () => (
      activeWorkspaceRef.current === sendWorkspace
      && workspaceEpochRef.current === sendWorkspaceEpoch
    );

    const stopBeforeRun = (notice: string) => {
      setSessionNotice(notice);
      setStatus('');
      setStreaming(false);
      sendInProgressRef.current = false;
    };

    const runBinding: WorkflowBinding = {
      workstreamId: selectedWorkstreamId,
      planId: selectedPlanId,
      planRevision: selectedPlanRevision,
      stageId: selectedStageId,
    };
    if (runBinding.planId) {
      if (!runBinding.workstreamId || runBinding.planRevision == null || !runBinding.stageId) {
        stopBeforeRun('Plan 실행에는 Workstream, 정확한 Plan revision, 단계가 모두 필요합니다.');
        return;
      }
      setStreaming(true);
      setStatus('Plan 상태 확인 중…');
      try {
        const currentPlan = await getWorkstreamPlan(
          runBinding.workstreamId,
          runBinding.planId,
          activeWorkspaceRef.current,
        );
        setPlans((current) => current.map((plan) => plan.id === currentPlan.id ? currentPlan : plan));
        if (currentPlan.revision !== runBinding.planRevision) {
          stopBeforeRun(`Plan이 revision ${currentPlan.revision}으로 변경됐습니다. 현재 revision을 다시 선택한 뒤 진행하세요.`);
          return;
        }
        if (!planIsApprovedCurrentRevision(currentPlan, runBinding.planRevision)) {
          stopBeforeRun('현재 Plan revision의 승인이 필요합니다. draft Plan을 승인한 뒤 다시 진행하세요.');
          return;
        }
        if (!planStageEligible(currentPlan, runBinding.stageId)) {
          stopBeforeRun('선택한 단계가 실행 가능한 상태가 아닙니다. 완료된 선행 단계와 현재 진행 상태를 확인하세요.');
          return;
        }
      } catch (err) {
        stopBeforeRun(`Plan 상태를 확인하지 못해 실행을 시작하지 않았습니다: ${err instanceof Error ? err.message : String(err)}`);
        return;
      }
    }

    if (!sendIsCurrent()) {
      sendInProgressRef.current = false;
      return;
    }

    runtimeSessionIdRef.current = null;
    setActiveApproval(null);
    setPlanReady(false); // any new turn clears the plan-approval prompt
    if (overrideText === undefined) setInput('');
    const displayText = attachedImage ? `${text} [image attached]` : text;
    const userMsg: ChatMessage = { role: 'user', content: displayText || '[image]', timestamp: new Date() };
    const newMsgs = [...messages, userMsg];
    setMessages([...newMsgs, { role: 'assistant', content: '', timestamp: new Date() }]);
    setStreaming(true);
    setExecutionPolicyPending(true);
    setStatus(t('chat.thinking'));
    // Start the liveness clock immediately (don't wait for the first backend
    // heartbeat at t=1s, and cover the connecting phase before any token).
    setElapsed(0);
    setGenChars(0);
    if (tickRef.current) clearInterval(tickRef.current);
    tickRef.current = setInterval(() => setElapsed((e) => e + 1), 1000);

    const msgContent = text || 'Analyze this image.';

    const apiMessages: AgentMessage[] = newMsgs
      .filter((m) => m.role === 'user' || m.role === 'assistant')
      .map((m) => ({ role: m.role, content: m.content } as AgentMessage));
    // Durable history stores only the display-safe label. The current request
    // carries image bytes in a canonical block alongside the user's text.
    if (apiMessages.length > 0) {
      apiMessages[apiMessages.length - 1] = {
        role: 'user',
        content: attachedImage
          ? [
              { type: 'text', text: msgContent },
              {
                type: 'image',
                source: { type: 'base64', media_type: attachedImage.mediaType, data: attachedImage.data },
              },
            ]
          : msgContent,
      };
    }

    setAttachedImage(null); // clear after sending

    // Abort controller so the user can interrupt a slow generation; aborting
    // the fetch cancels the request context, which propagates to RunLoop and
    // stops the provider stream server-side.
    const controller = new AbortController();
    abortRef.current = controller;
    const streamIsCurrent = () => (
      activeWorkspaceRef.current === sendWorkspace
      && workspaceEpochRef.current === sendWorkspaceEpoch
    );
    let streamEnded = false;
    let streamFailed = false;
    let durableHandled = false;

    try {
      await persistSession(newMsgs, runBinding);
      if (!streamIsCurrent()) {
        controller.abort();
        throw new DOMException('The operation was aborted.', 'AbortError');
      }
      const durableSessionId = durableSessionIDRef.current;
      const expectedRevision = durableSessionRevisionRef.current;
      if (!durableSessionId || expectedRevision == null) {
        throw new Error('Durable session binding is unavailable');
      }
      // Only an explicit user choice is sent; otherwise the session's stored
      // policy or the server default applies (see lib/executionMode.ts).
      const preferredMode = loadPreferredExecutionMode();
      const res = await fetch('/api/agent', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: apiMessages,
          workstreamId: runBinding.workstreamId || undefined,
          durableSessionId,
          expectedRevision,
          executionPolicy: preferredMode ? { mode: preferredMode } : undefined,
        }),
        signal: controller.signal,
      });

      if (!res.ok) {
        let message = `Agent request failed (${res.status}).`;
        try {
          const payload = await res.json() as { error?: { message?: string } };
          if (payload.error?.message) message = payload.error.message;
        } catch { /* retain the status fallback */ }
        throw new Error(message);
      }
      if (!res.body) throw new Error('Agent response stream is unavailable.');
      for await (const frame of streamSSE(res, controller.signal)) {
        if (!streamIsCurrent()) {
          controller.abort();
          break;
        }
        try {
          const event = JSON.parse(frame.data) as AgentEvent;
          if (event.type === 'done' || event.type === 'stream_end') streamEnded = true;
          if (event.type === 'done' && completionBlocksSuccess(eventObject(event.data))) streamFailed = true;
          if (event.type === 'error') streamFailed = true;
          if (event.type === 'durable_session' || event.type === 'durable_session_error') durableHandled = true;
          handleAgentEvent(event);
        } catch { /* skip */ }
      }
      if (streamIsCurrent() && !controller.signal.aborted && !streamEnded && !streamFailed) {
        streamFailed = true;
        setSessionNotice('The connection ended before completion was confirmed. Reload this session to check its saved state before retrying.');
        throw new Error('Connection interrupted before the run completed. The partial response is preserved.');
      }
    } catch (err) {
      const aborted = err instanceof DOMException && err.name === 'AbortError';
      streamFailed = true;
      if (streamIsCurrent()) {
        setMessages((prev) => {
          const updated = [...prev];
          const last = updated[updated.length - 1];
          if (last?.role === 'assistant') {
            const note = aborted ? '\n\n_(stopped)_' : `\n\n[Error: ${err}]`;
            updated[updated.length - 1] = { ...last, content: last.content + note };
          }
          return updated;
        });
      }
    } finally {
      runtimeSessionIdRef.current = null;
      if (controller.signal.aborted || streamFailed || !streamEnded) {
        setActiveApproval(null);
      } else {
        setActiveApproval((current) => (
          current && (current.state === 'pending' || current.state === 'submitting' || current.state === 'error')
            ? null
            : current
        ));
      }
      setStreaming(false);
      setExecutionPolicyPending(false);
      sendInProgressRef.current = false;
      setStatus('');
      abortRef.current = null;
      if (tickRef.current) { clearInterval(tickRef.current); tickRef.current = null; }
      inputRef.current?.focus();
      // Compatibility fallback for an older backend that does not own the
      // durable run checkpoint. Never overwrite an interrupted/conflicted run.
      if (streamIsCurrent() && !controller.signal.aborted && !streamFailed && streamEnded && !durableHandled) {
        setMessages((prev) => {
          void persistSession(prev).catch(() => undefined);
          return prev;
        });
      }
    }
  }

  async function decideApproval(decision: ApprovalDecision) {
    const approval = activeApproval;
    if (!approval || approval.state === 'submitting' || approval.state === 'resolved' || approval.state === 'expired') {
      return;
    }
    if (runtimeSessionIdRef.current !== approval.runtimeSessionId) {
      setActiveApproval({
        ...approval,
        state: 'error',
        error: 'The active run changed. This request was not approved.',
      });
      abortRef.current?.abort();
      return;
    }

    setActiveApproval({ ...approval, state: 'submitting', decision, error: undefined });
    try {
      await resolveApproval(approval.id, approval.runtimeSessionId, decision, abortRef.current?.signal);
      setActiveApproval((current) => current?.id === approval.id
        ? { ...current, state: 'resolved', decision, error: undefined }
        : current);
    } catch (err) {
      if (abortRef.current?.signal.aborted) return;
      const noLongerActionable = err instanceof HTTPError && (err.status === 404 || err.status === 409);
      setActiveApproval((current) => current?.id === approval.id
        ? {
            ...current,
            state: noLongerActionable ? 'expired' : 'error',
            error: noLongerActionable
              ? 'This request is no longer actionable. Nothing was approved.'
              : 'Approval could not be recorded. Nothing was approved; retry or deny.',
          }
        : current);
    }
  }

  function handleAgentEvent(event: AgentEvent) {
    const data = eventObject(event.data);
    switch (event.type) {
      case 'session':
        if (typeof data.sessionId === 'string' && data.sessionId.trim()) {
          runtimeSessionIdRef.current = data.sessionId;
        }
        if (typeof data.durableSessionId === 'string' && typeof data.durableRevision === 'number') {
          durableSessionIDRef.current = data.durableSessionId;
          durableSessionRevisionRef.current = data.durableRevision;
          setCurrentSession((current) => ({
            id: data.durableSessionId as string,
            title: current && current.id === data.durableSessionId ? current.title : 'Current session',
            revision: data.durableRevision as number,
            lifecycleStatus: current && current.id === data.durableSessionId ? current.lifecycleStatus : 'active',
          }));
        }
        setEffectiveExecutionPolicy(readExecutionPolicy(data.executionPolicy));
        setExecutionPolicyPending(false);
        break;
      case 'durable_session':
        if (typeof data.sessionId === 'string' && typeof data.revision === 'number') {
          durableSessionIDRef.current = data.sessionId;
          durableSessionRevisionRef.current = data.revision;
          setCurrentSession((current) => ({
            id: data.sessionId as string,
            title: current && current.id === data.sessionId ? current.title : 'Current session',
            revision: data.revision as number,
            lifecycleStatus: data.reconcileRequired ? 'recovery-needed' : 'active',
          }));
          setSessionNotice(data.reconcileRequired
            ? 'This session was interrupted after a tool started. Reconcile it before resuming.'
            : '');
          listSessions(activeWorkspaceRef.current).then((sessions) => setSessions(sessions || [])).catch(() => setSessions([]));
        }
        break;
      case 'durable_session_error':
        setSessionNotice(`Durable session checkpoint failed${data.code ? ` (${data.code})` : ''}: ${data.message || 'reload before continuing'}`);
        break;
      case 'undo_preview': {
        const entries = readUndoEntries(data.entries);
        setUndoEntries(entries);
        setUndoActionBusy(false);
        if (entries.length > 0) {
          setInspectorOpen(true);
          setInspectorTab('changes');
        }
        if (typeof data.error === 'string' && data.error) setSessionNotice(data.error);
        break;
      }
      case 'undo_result':
        setUndoEntries(readUndoEntries(data.entries));
        setUndoActionBusy(false);
        if (typeof data.error === 'string' && data.error) setSessionNotice(data.error);
        break;
      case 'approval_required': {
        const runtimeSessionId = runtimeSessionIdRef.current;
        const eventSessionId = typeof data.sessionId === 'string' ? data.sessionId : '';
        if (!runtimeSessionId || !data.id || !data.toolName || (eventSessionId && eventSessionId !== runtimeSessionId)) {
          setMessages((prev) => [...prev, {
            role: 'assistant',
            content: 'Error: approval request could not be bound to the active run; nothing was approved.',
            timestamp: new Date(),
          }]);
          abortRef.current?.abort();
          break;
        }
        setStatus('');
        // tool_input precedes approval_required in the stream. Remove that raw
        // payload from the live transcript before it can be persisted; the
        // approval row below renders only the server-provided redacted view.
        setMessages((prev) => {
          const index = findOpenToolMessage(prev, data.toolCallId, data.toolName);
          if (index < 0) return prev;
          const updated = [...prev];
          updated[index] = { ...updated[index], toolInput: undefined };
          return updated;
        });
        setActiveApproval({
          id: data.id,
          runtimeSessionId,
          toolName: data.toolName,
          toolCallId: typeof data.toolCallId === 'string' && data.toolCallId ? data.toolCallId : undefined,
          redactedInput: typeof data.redactedInput === 'string' ? data.redactedInput : 'Input details unavailable',
          dangerLevel: data.dangerLevel,
          scope: data.scope,
          expiresAt: data.expiresAt,
          state: 'pending',
        });
        break;
      }
      case 'thinking':
        // Append thinking text to assistant message (wrapped in <think> tags for rendering)
        setMessages((prev) => {
          const updated = [...prev];
          const last = updated[updated.length - 1];
          if (last?.role === 'assistant') {
            const thinkTag = last.content.includes('<think>') ? '' : '<think>';
            updated[updated.length - 1] = { ...last, content: last.content + thinkTag + eventText(event.data) };
          }
          return updated;
        });
        break;
      case 'text':
        setMessages((prev) => {
          const updated = [...prev];
          const last = updated[updated.length - 1];
          if (last?.role === 'assistant') {
            // Close thinking block if transitioning to text
            const closingTag = last.content.includes('<think>') && !last.content.includes('</think>') ? '</think>\n\n' : '';
            updated[updated.length - 1] = { ...last, content: last.content + closingTag + eventText(event.data) };
          }
          return updated;
        });
        break;
      case 'tool_start':
        setStatus(`${t('chat.executing')} ${data.name || ''}`);
        break;
      case 'tool_input':
        setMessages((prev) => [...prev, {
          role: 'tool' as const, content: '', toolName: data.name,
          toolCallId: typeof data.id === 'string' && data.id ? data.id : undefined,
          toolInput: data.input, timestamp: new Date(),
        }]);
        break;
      case 'tool_result':
        setMessages((prev) => {
          const toolCallId = typeof data.id === 'string' && data.id ? data.id : undefined;
          const index = findOpenToolMessage(prev, toolCallId, data.name);
          const result = typeof data.result === 'string' ? data.result : '';
          if (index < 0) {
            // Calls rejected before execution (read-only or plan mode) emit no
            // tool_input; show the result as its own card instead of dropping it.
            return [...prev, {
              role: 'tool' as const, content: '', toolName: data.name, toolCallId,
              toolResult: result, isError: data.isError, timestamp: new Date(),
            }];
          }
          const updated = [...prev];
          updated[index] = { ...updated[index], toolResult: result, isError: data.isError };
          return updated;
        });
        setStatus('');
        setMessages((prev) => [...prev, { role: 'assistant', content: '', timestamp: new Date() }]);
        break;
      case 'diff': {
        // Attach the before/after diff to the most recent Edit/Write tool card.
        const diffFile = data.file;
        const diffText = data.diff;
        if (typeof diffFile === 'string' && typeof diffText === 'string') {
          setDiffRecords((current) => [...current, {
            turn: turnNumberRef.current,
            file: diffFile,
            diff: diffText,
          }].slice(-200));
          setInspectorOpen(true);
          setInspectorTab('changes');
        }
        setMessages((prev) => {
          const updated = [...prev];
          for (let i = updated.length - 1; i >= 0; i--) {
            if (updated[i].role === 'tool' && !updated[i].toolDiff &&
                (updated[i].toolName === 'Edit' || updated[i].toolName === 'Write')) {
              updated[i] = { ...updated[i], toolDiff: data.diff };
              break;
            }
          }
          return updated;
        });
        break;
      }
      case 'heartbeat':
        // Authoritative output size + elapsed from the backend liveness ticker.
        if (typeof data.chars === 'number') setGenChars(data.chars);
        if (typeof data.elapsedMs === 'number') setElapsed(Math.floor(data.elapsedMs / 1000));
        break;
      case 'workstream':
        if (typeof data.id === 'string') {
          setSelectedWorkstreamId(data.id);
        }
        break;
      case 'status':
        setStatus(eventText(event.data));
        break;
      case 'done':
      case 'stream_end':
        if (event.type === 'done') {
          if (data && typeof data === 'object') {
            const raw = data as Record<string, unknown>;
            const receiptPath = typeof raw.receipt === 'string'
              ? raw.receipt
              : (typeof raw.receiptPath === 'string' ? raw.receiptPath : undefined);
            // Only the server's verification result counts as a verification
            // status; the terminal state is presented separately by describeReceipt.
            const receipt: RunReceipt = {
              terminalState: typeof raw.terminalState === 'string' ? raw.terminalState : undefined,
              kind: typeof raw.kind === 'string' ? raw.kind : undefined,
              stopReason: typeof raw.stopReason === 'string' ? raw.stopReason : undefined,
              completionStatus: typeof raw.completionStatus === 'string' ? raw.completionStatus : undefined,
              completionRevision: typeof raw.completionRevision === 'number' ? raw.completionRevision : undefined,
              // The server reports the number of blocked completion criteria.
              completionBlocked: raw.completionBlocked === true ||
                (typeof raw.completionBlocked === 'number' && raw.completionBlocked > 0),
              verificationStatus: typeof raw.verificationStatus === 'string' ? raw.verificationStatus : undefined,
              receiptPath: receiptPath,
              timestamp: new Date(),
            };
            setLatestReceipt(receipt);
            setMessages((prev) => {
              if (prev.length === 0) return prev;
              const lastIdx = prev.length - 1;
              const lastMsg = prev[lastIdx];
              if (lastMsg && lastMsg.role === 'assistant') {
                const next = [...prev];
                next[lastIdx] = { ...lastMsg, receipt };
                return next;
              }
              return prev;
            });
          }
          if (completionBlocksSuccess(data)) {
            const status = data.completionStatus || data.terminalState || 'blocked';
            const revision = typeof data.completionRevision === 'number' ? ` at revision ${data.completionRevision}` : '';
            setSessionNotice(`Run ended without successful completion: ${status}${revision}. Continue or reconcile this session.`);
          } else if (data.planMode) {
            setPlanReady(true);
          }
        }
        setStatus('');
        setMessages((prev) => {
          if (prev[prev.length - 1]?.role === 'assistant' && prev[prev.length - 1]?.content === '') {
            return prev.slice(0, -1);
          }
          return prev;
        });
        refreshWorkstreams();
        void refreshPlans();
        break;
      case 'error':
        setMessages((prev) => [...prev, { role: 'assistant', content: `Error: ${eventText(event.data)}`, timestamp: new Date() }]);
        break;
    }
  }

  // "Thinking… · 12s · 340 chars" — a moving clock + growing output size is the
  // canonical "it's alive" signal for a slow local model (Claude Code style).
  const approvalNeedsAction = activeApproval != null &&
    (activeApproval.state === 'pending' || activeApproval.state === 'submitting' || activeApproval.state === 'error');
  const approvalCardIndex = activeApproval
    ? findOpenToolMessage(messages, activeApproval.toolCallId, activeApproval.toolName)
    : -1;

  // Let the app shell show a pending decision while this page is hidden.
  useEffect(() => {
    onApprovalPendingChange?.(approvalNeedsAction);
  }, [approvalNeedsAction, onApprovalPendingChange]);
  const liveLabel = streaming
    ? `${status || 'Thinking…'} · ${elapsed}s${genChars > 0 ? ` · ${genChars.toLocaleString()} chars` : ''}`
    : '';
  const selectedPlan = plans.find((plan) => plan.id === selectedPlanId);
  const selectedStage = selectedPlan?.stages.find((stage) => stage.id === selectedStageId);
  const changedFiles = Array.from(new Set(diffRecords.map((record) => record.file).filter(Boolean)));
  const currentTurnDiffs = diffRecords.filter((record) => record.turn === turnNumberRef.current);
  const permissionLabel = executionPolicyPending
    ? 'checking…'
    : effectiveExecutionPolicy
      ? executionModeLabel(effectiveExecutionPolicy.mode)
      : 'unknown';
  const persistedBinding = persistedWorkflowBindingRef.current;
  const cannotClearWorkstream = Boolean(durableSessionIDRef.current && persistedBinding.workstreamId);
  const cannotClearPlan = Boolean(
    durableSessionIDRef.current && selectedWorkstreamId &&
    persistedBinding.workstreamId === selectedWorkstreamId && persistedBinding.planId,
  );

  function changeWorkstream(workstreamId: string) {
    const persisted = persistedWorkflowBindingRef.current;
    const binding = workstreamId && workstreamId === persisted.workstreamId
      ? { ...persisted }
      : { workstreamId, planId: '', planRevision: undefined, stageId: '' };
    setSelectedWorkstreamId(workstreamId);
    setSelectedPlanId(binding.planId);
    setSelectedPlanRevision(binding.planRevision);
    setSelectedStageId(binding.stageId);
    void persistSession(messages, binding).catch(() => undefined);
  }

  function changePlan(planId: string) {
    if (!planId && cannotClearPlan) return;
    const plan = plans.find((item) => item.id === planId);
    const stage = plan?.stages.find((item) => planStageEligible(plan, item.id)) || plan?.stages[0];
    const binding: WorkflowBinding = {
      workstreamId: selectedWorkstreamId,
      planId,
      planRevision: plan?.revision,
      stageId: stage?.id || '',
    };
    setSelectedPlanId(planId);
    setSelectedPlanRevision(binding.planRevision);
    setSelectedStageId(binding.stageId);
    void persistSession(messages, binding).catch(() => undefined);
  }

  function changeStage(stageId: string) {
    const binding: WorkflowBinding = {
      workstreamId: selectedWorkstreamId,
      planId: selectedPlanId,
      planRevision: selectedPlanRevision,
      stageId,
    };
    setSelectedStageId(stageId);
    void persistSession(messages, binding).catch(() => undefined);
  }

  function bindCurrentPlanRevision() {
    if (!selectedWorkstreamId || !selectedPlan) return;
    const stage = selectedPlan.stages.find((item) => planStageEligible(selectedPlan, item.id)) || selectedPlan.stages[0];
    const binding: WorkflowBinding = {
      workstreamId: selectedWorkstreamId,
      planId: selectedPlan.id,
      planRevision: selectedPlan.revision,
      stageId: stage?.id || '',
    };
    setSelectedPlanRevision(binding.planRevision);
    setSelectedStageId(binding.stageId);
    void persistSession(messages, binding).catch(() => undefined);
    setSessionNotice(`Plan revision ${selectedPlan.revision}을 이 대화에 연결했습니다.`);
  }

  async function approveSelectedPlan() {
    const plan = selectedPlan;
    if (!selectedWorkstreamId || !plan || plan.status !== 'draft' || !plan.revision || !plan.stateRevision || streaming) return;
    setPlanActionBusy(true);
    try {
      const approved = await approveWorkstreamPlan(
        selectedWorkstreamId,
        plan.id,
        activeWorkspaceRef.current,
        plan.revision,
        plan.stateRevision,
      );
      setPlans((current) => current.map((item) => item.id === approved.id ? approved : item));
      setSelectedPlanRevision(approved.revision);
      setSessionNotice(`Plan revision ${approved.revision} 승인 완료`);
    } catch (err) {
      setSessionNotice(`Plan 승인에 실패했습니다: ${err instanceof Error ? err.message : String(err)}. 최신 상태를 다시 불러옵니다.`);
      await refreshPlans(selectedWorkstreamId, activeWorkspaceRef.current);
    } finally {
      setPlanActionBusy(false);
    }
  }

  async function reconcileSelectedPlanStage() {
    const plan = selectedPlan;
    const stage = plan?.stages.find((item) => item.id === selectedStageId);
    const attempts = stage?.attempts ?? [];
    const attempt = attempts[attempts.length - 1];
    // The Inspector's inline confirmation is the single confirmation step.
    if (!selectedWorkstreamId || !plan || !stage || stage.status !== 'running' || !attempt || streaming || planActionBusy) return;
    setPlanActionBusy(true);
    try {
      const reconciled = await reconcileWorkstreamPlanStage(
        selectedWorkstreamId,
        plan.id,
        stage.id,
        activeWorkspaceRef.current,
        plan.revision,
        plan.stateRevision,
        attempt.runId,
      );
      setPlans((current) => current.map((item) => item.id === reconciled.id ? reconciled : item));
      setSessionNotice(`Stage ${stage.id} was reconciled as incomplete. It can be retried after reviewing its changes.`);
    } catch (err) {
      setSessionNotice(`Stage recovery failed: ${err instanceof Error ? err.message : String(err)}. Latest Plan state is being reloaded.`);
      await refreshPlans(selectedWorkstreamId, activeWorkspaceRef.current);
    } finally {
      setPlanActionBusy(false);
    }
  }

  async function createCurrentWorkstream() {
    const seed = input.trim() || messages.find((m) => m.role === 'user')?.content || 'Local agent workstream';
    const title = seed.split('\n')[0].slice(0, 80) || 'Local agent workstream';
    try {
      const created = await createWorkstream({
        workDir: activeWorkspaceRef.current,
        title,
        summary: seed,
        nextAction: seed,
        tags: ['chat'],
        goal: {
          objective: seed,
          acceptanceCriteria: ['Agent runs are linked to this workstream', 'Verification and receipts are recorded'],
          verificationPolicy: { requiredSignals: ['test'], maxRepairAttempts: 2 },
        },
      });
      changeWorkstream(created.id);
      setWorkstreamNotice(`Created ${created.title}`);
      await refreshWorkstreams();
    } catch (err) {
      setWorkstreamNotice(err instanceof Error ? err.message : String(err));
    }
  }

  async function exportHandoff() {
    if (!selectedWorkstreamId) return;
    try {
      const result = await generateHandoff(selectedWorkstreamId, activeWorkspaceRef.current, selectedPlanId || undefined);
      setWorkstreamNotice(`Handoff saved: ${result.path}`);
    } catch (err) {
      setWorkstreamNotice(err instanceof Error ? err.message : String(err));
    }
  }

  function handleCheckRestore() {
    setUndoActionBusy(true);
    void send('/undo --list');
  }

  function handleRestoreUndoEntry(id: string) {
    setUndoActionBusy(true);
    void send(`/undo --select ${id}`);
  }

  return (
    <div className="flex flex-col flex-1 min-w-0 h-full w-full overflow-hidden">
      {/* Header */}
      <header className="px-4 py-2 border-b border-[var(--color-border)] bg-[var(--color-surface)] flex items-center justify-between gap-3">
        <div className="flex flex-wrap items-center gap-2 min-w-0">
          <div className="flex items-center gap-1.5 font-medium text-xs text-[var(--color-text)]">
            <FolderGit2 className="w-3.5 h-3.5 text-[var(--color-accent)] flex-shrink-0" />
            <span title={activeWorkspace || 'No project'}>
              <span className="text-[var(--color-text)]">Project</span> {workspaceLabel(activeWorkspace)}
            </span>
          </div>

          {gitBranch && (
            <span className="flex items-center gap-1 text-[11px] font-mono text-[var(--color-text2)] bg-[var(--color-bg)] px-1.5 py-0.5 rounded border border-[var(--color-border)]">
              <GitBranch className="w-3 h-3 text-[var(--color-text2)]" />
              <span>{gitBranch}</span>
            </span>
          )}

          <span
            className="px-2 py-0.5 rounded text-[11px] font-mono bg-[var(--color-bg)] border border-[var(--color-border)] text-[var(--color-text)]"
            title={effectiveExecutionPolicy ? `Effective execution mode, revision ${effectiveExecutionPolicy.revision}` : 'Effective execution mode has not been confirmed'}
          >
            <span className="text-[var(--color-text2)]">Permission</span> {permissionLabel}
          </span>

          {approvalNeedsAction && (
            <button
              type="button"
              onClick={() => {
                // The deny button belongs to the linked step card or, without a
                // card, to the fallback panel; both are the decision point.
                const pendingCard = document.querySelector('[data-pending-approval="true"]');
                (pendingCard ?? denyApprovalRef.current)?.scrollIntoView({ behavior: 'smooth', block: 'center' });
                denyApprovalRef.current?.focus({ preventScroll: true });
              }}
              className="flex items-center gap-1 text-[var(--color-warning)] border border-[var(--color-warning)]/50 px-2 py-0.5 rounded-full text-[11px] font-medium animate-pulse"
              title="승인 대기 중인 도구 호출이 있습니다"
            >
              <ShieldAlert aria-hidden="true" className="w-3 h-3" />
              <span>승인 필요</span>
            </button>
          )}

          {selectedPlan && (selectedPlan.status === 'draft' || (selectedPlan.approvedRevision !== selectedPlan.revision)) && (
            <button
              type="button"
              onClick={() => {
                setInspectorTab('workflow');
                setInspectorOpen(true);
              }}
              className="flex items-center gap-1 text-[var(--color-warning)] bg-[var(--color-warning)]/15 border border-[var(--color-warning)]/40 px-2 py-0.5 rounded-full text-[11px] font-medium"
              title="Workstream Plan 승인이 필요합니다"
            >
              <ShieldAlert className="w-3 h-3" />
              <span>Plan 승인 필요</span>
            </button>
          )}

          {(selectedStage?.status === 'running' || (sessionNotice && sessionNotice.includes('reconcile'))) && (
            <button
              type="button"
              onClick={() => {
                setInspectorTab('workflow');
                setInspectorOpen(true);
              }}
              className="flex items-center gap-1 text-[var(--color-warning)] bg-[var(--color-warning)]/15 border border-[var(--color-warning)]/40 px-2 py-0.5 rounded-full text-[11px] font-medium"
              title="중단된 단계가 있어 복구가 필요합니다"
            >
              <RotateCcw className="w-3 h-3" />
              <span>복구 필요</span>
            </button>
          )}

          {streaming ? (
            <div className="flex items-center gap-1.5 text-[var(--color-accent)] text-xs">
              <div className="w-2 h-2 rounded-full bg-[var(--color-accent)] animate-pulse" />
              {liveLabel}
            </div>
          ) : status ? (
            <div className="flex items-center gap-1.5 text-[var(--color-accent)] text-xs">
              <div className="w-2 h-2 rounded-full bg-[var(--color-accent)] animate-pulse" />
              {status}
            </div>
          ) : null}
        </div>

        <div className="flex items-center gap-2 flex-shrink-0">
          <span className="text-xs text-[var(--color-text2)] hidden sm:inline">
            {messages.length > 0 ? `${messages.filter(m => m.role === 'user').length} messages` : 'New conversation'}
          </span>

          <button
            type="button"
            onClick={() => setInspectorOpen((prev) => !prev)}
            aria-expanded={inspectorOpen}
            className={`flex items-center gap-1.5 px-2.5 py-1.5 rounded-lg border text-xs font-medium transition-colors ${
              inspectorOpen
                ? 'bg-[var(--color-accent)]/15 border-[var(--color-accent)] text-[var(--color-accent)]'
                : 'border-[var(--color-border)] text-[var(--color-text)] hover:border-[var(--color-accent)]'
            }`}
            title="Toggle Inspector drawer"
          >
            <SlidersHorizontal className="w-3.5 h-3.5" />
            <span>Inspector</span>
            {(changedFiles.length > 0 || undoEntries.length > 0 || selectedPlan != null) && (
              <span className="w-1.5 h-1.5 rounded-full bg-[var(--color-accent)]" />
            )}
          </button>
        </div>
      </header>

      {(workstreamNotice || sessionNotice) && (
        <div className="px-4 py-1.5 border-b border-[var(--color-border)] bg-[var(--color-bg)] flex items-center justify-between text-xs text-[var(--color-text2)]">
          <span className="truncate">{sessionNotice || workstreamNotice}</span>
          <button
            type="button"
            onClick={() => { setSessionNotice(''); setWorkstreamNotice(''); }}
            className="hover:text-[var(--color-text)] ml-2 flex-shrink-0"
            title="Dismiss notice"
          >
            <X className="w-3.5 h-3.5" />
          </button>
        </div>
      )}

      {/* Main Layout Area: Chat column on the left, Inspector on the right */}
      <div className="flex flex-1 min-h-0 w-full overflow-hidden">
        <div className="flex flex-col flex-1 min-w-0 h-full overflow-hidden">
          {/* Messages Area — full width */}
          <div className="flex-1 overflow-y-auto px-6 py-4 space-y-3">
            {messages.length === 0 && (
              <div className="flex flex-col items-center justify-center h-full text-[var(--color-text2)] max-w-xl mx-auto px-4 py-8">
                {/* Logo & Emblem */}
                <div className="relative mb-5 flex items-center justify-center">
                  <div className="w-14 h-14 rounded-2xl bg-gradient-to-br from-[var(--color-accent)] to-[var(--color-accent2)] flex items-center justify-center text-[var(--color-bg)] text-xl font-bold shadow-lg shadow-[var(--color-accent)]/20 ring-4 ring-[var(--color-accent)]/10">
                    C
                  </div>
                </div>

                <h2 className="text-xl font-semibold mb-2 text-[var(--color-text)] text-center tracking-tight">
                  {ko ? '어떤 작업을 진행할까요?' : 'What would you like to build today?'}
                </h2>
                <p className="text-xs text-[var(--color-text2)] mb-6 text-center max-w-md leading-relaxed">
                  {ko
                    ? '프로젝트 분석부터 새 기능 구현, 대화형 계획(/plan), 실시간 diff 검증까지 지원합니다.'
                    : 'Analyze project architecture, implement new features, create staged plans, and verify diffs in real-time.'}
                </p>

                {/* 2x2 Prompt Starters Grid with Icons */}
                <div className="grid grid-cols-1 sm:grid-cols-2 gap-3 w-full mb-6">
                  {[
                    {
                      icon: Compass,
                      title: ko ? '프로젝트 구조 분석' : 'Analyze Architecture',
                      desc: ko ? '주요 아키텍처와 흐름 파악' : 'Understand key flows & components',
                      prompt: ko ? '이 프로젝트의 핵심 구조와 아키텍처를 분석해줘.' : 'Analyze the core architecture and key components of this project.',
                    },
                    {
                      icon: Layers,
                      title: ko ? '대화형 계획 수립' : 'Create Plan (/plan)',
                      desc: ko ? '요구사항 인터뷰와 단계별 플랜' : 'Interactive interview & staged steps',
                      prompt: '/plan ',
                    },
                    {
                      icon: Bug,
                      title: ko ? '결함 탐지 및 검증' : 'Find Bugs & Verify',
                      desc: ko ? '잠재적 버그 점검 및 테스트 실행' : 'Inspect edge cases and run tests',
                      prompt: ko ? '현재 코드베이스의 잠재적 결함을 점검하고 테스트를 실행해줘.' : 'Check for potential bugs in this codebase and run relevant tests.',
                    },
                    {
                      icon: CodeXml,
                      title: ko ? '새 기능 구현 및 리팩토링' : 'Implement & Refactor',
                      desc: ko ? '안전한 파일 수정 및 diff 확인' : 'Safe edits with live diff inspection',
                      prompt: ko ? '새로운 기능을 추가하거나 기존 코드를 깔끔하게 리팩토링해줘.' : 'Implement a new feature or refactor existing code cleanly.',
                    },
                  ].map((card, idx) => {
                    const IconComp = card.icon;
                    return (
                      <button
                        key={idx}
                        type="button"
                        onClick={() => {
                          setInput(card.prompt);
                          inputRef.current?.focus();
                        }}
                        className="text-left p-3.5 bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl hover:border-[var(--color-accent)] hover:shadow-sm transition-all group"
                      >
                        <div className="flex items-center gap-2 mb-1.5">
                          <div className="p-1.5 rounded-lg bg-[var(--color-surface2)] text-[var(--color-accent)] group-hover:bg-[var(--color-accent)] group-hover:text-[var(--color-bg)] transition-colors">
                            <IconComp className="w-3.5 h-3.5" />
                          </div>
                          <div className="text-xs font-semibold text-[var(--color-text)] group-hover:text-[var(--color-accent)] transition-colors">
                            {card.title}
                          </div>
                        </div>
                        <div className="text-[11px] text-[var(--color-text2)] leading-snug">
                          {card.desc}
                        </div>
                      </button>
                    );
                  })}
                </div>

                {/* Quick Command Shortcuts */}
                <div className="flex flex-wrap items-center justify-center gap-1.5 text-xs text-[var(--color-text2)]">
                  <span className="text-[11px] mr-1">{ko ? '빠른 명령:' : 'Quick commands:'}</span>
                  {['/plan', '/undo', '/help'].map((cmd) => (
                    <button
                      key={cmd}
                      type="button"
                      onClick={() => {
                        setInput(cmd + ' ');
                        inputRef.current?.focus();
                      }}
                      className="px-2 py-0.5 rounded-md font-mono text-[11px] bg-[var(--color-surface2)] border border-[var(--color-border)] text-[var(--color-accent)] hover:border-[var(--color-accent)] transition-colors"
                    >
                      {cmd}
                    </button>
                  ))}
                </div>
              </div>
            )}

            {messages.map((msg, i) => {
              if (msg.role === 'user') {
                return (
                  <div key={i} className="flex justify-end">
                    <div className="max-w-[85%] bg-[var(--color-accent)] text-[var(--color-bg)] rounded-xl rounded-br-sm px-4 py-3 text-sm">
                      <div className="whitespace-pre-wrap">{msg.content}</div>
                      <div className="text-[10px] text-[var(--color-bg)]/70 mt-1">{msg.timestamp.toLocaleTimeString()}</div>
                    </div>
                  </div>
                );
              }
              if (msg.role === 'tool') {
                const linkedApproval = i === approvalCardIndex ? activeApproval : null;
                return (
                  <StepCard
                    key={i}
                    toolName={msg.toolName}
                    toolInput={msg.toolInput}
                    toolResult={msg.toolResult}
                    toolDiff={msg.toolDiff}
                    isError={msg.isError}
                    timestamp={msg.timestamp}
                    activeApproval={linkedApproval}
                    onResolveApproval={decideApproval}
                    denyRef={linkedApproval && approvalNeedsAction ? denyApprovalRef : undefined}
                    runActive={streaming}
                  />
                );
              }
              if (msg.content === '' && streaming) {
                return (
                  <div key={i} className="flex justify-start">
                    <div className="bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl rounded-bl-sm px-4 py-3 flex items-center gap-2.5">
                      <div className="flex gap-1">
                        <div className="w-2 h-2 bg-[var(--color-text2)] rounded-full animate-bounce" style={{ animationDelay: '0ms' }} />
                        <div className="w-2 h-2 bg-[var(--color-text2)] rounded-full animate-bounce" style={{ animationDelay: '150ms' }} />
                        <div className="w-2 h-2 bg-[var(--color-text2)] rounded-full animate-bounce" style={{ animationDelay: '300ms' }} />
                      </div>
                      {/* Proof-of-life while the model is still on its first token */}
                      <span className="text-[11px] text-[var(--color-text2)] tabular-nums">
                        {elapsed}s{genChars > 0 ? ` · ${genChars.toLocaleString()} chars` : ''}
                      </span>
                    </div>
                  </div>
                );
              }
              if (msg.content === '') return null;
              const lastAssistantIdx = messages.reduce((last, m, idx) => m.role === 'assistant' ? idx : last, -1);
              const isLastAssistant = i === lastAssistantIdx;
              const receipt = msg.receipt || (isLastAssistant && !streaming ? latestReceipt : undefined);
              return (
                <div key={i} className="flex justify-start group">
                  <div className="max-w-[95%] bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl rounded-bl-sm px-4 py-3 text-sm">
                    <div className="chat-md"><Markdown content={msg.content} /></div>
                    <div className="flex gap-1 mt-1.5 opacity-0 group-hover:opacity-100 transition-opacity">
                      <button
                        onClick={() => fetch('/api/feedback', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ messageId: i, rating: 'up', model: 'auto' }) })}
                        className="text-[10px] px-1.5 py-0.5 rounded hover:bg-[var(--color-surface2)] text-[var(--color-text2)]"
                        title="Good response"
                      >+1</button>
                      <button
                        onClick={() => fetch('/api/feedback', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ messageId: i, rating: 'down', model: 'auto' }) })}
                        className="text-[10px] px-1.5 py-0.5 rounded hover:bg-[var(--color-surface2)] text-[var(--color-text2)]"
                        title="Bad response"
                      >-1</button>
                    </div>

                    {/* Inline Turn Verification Receipt */}
                    {receipt && (
                      <div className="mt-3 pt-2.5 border-t border-[var(--color-border)] flex flex-wrap items-center justify-between gap-2 text-xs">
                        <div className="flex items-center gap-2">
                          <span className="text-[10px] uppercase font-semibold text-[var(--color-text2)]">Turn Verification</span>
                          <ReceiptBadge receipt={receipt} />
                        </div>
                        <button
                          type="button"
                          onClick={() => {
                            setInspectorTab('receipt');
                            setInspectorOpen(true);
                          }}
                          className="inline-flex items-center gap-1 text-[11px] text-[var(--color-accent)] hover:underline"
                        >
                          Receipt details
                          <ExternalLink className="w-3 h-3" />
                        </button>
                      </div>
                    )}
                  </div>
                </div>
              );
            })}
            {/* An expiry with no card to carry it is still reported, quietly. */}
            {activeApproval && activeApproval.state === 'expired' && approvalCardIndex < 0 && (
              <div role="status" className="mx-0 sm:mx-4 px-3 py-2 rounded-lg border border-[var(--color-border)] text-xs text-[var(--color-text2)]">
                <span className="font-mono">{activeApproval.toolName}</span> 승인 요청이 만료되어 아무것도 허용되지 않았습니다.
              </div>
            )}
            {/* Fallback only when a decision is still needed and no step card is linked. */}
            {activeApproval && approvalNeedsAction && approvalCardIndex < 0 && (
              <div
                role="alert"
                aria-busy={activeApproval.state === 'submitting'}
                className="mx-0 sm:mx-4 border border-[var(--color-warning)] bg-[var(--color-surface2)] rounded-lg px-3 py-3"
              >
                <div className="flex items-start gap-2.5">
                  <ShieldAlert aria-hidden="true" className="w-4 h-4 mt-0.5 shrink-0 text-[var(--color-warning)]" />
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                      <span className="text-xs font-semibold text-[var(--color-text)]">Permission required</span>
                      <span className="text-xs font-mono text-[var(--color-text)] break-all">{activeApproval.toolName}</span>
                      {activeApproval.dangerLevel && (
                        <span className="text-[10px] uppercase text-[var(--color-warning)]">risk: {activeApproval.dangerLevel}</span>
                      )}
                      {activeApproval.scope && (
                        <span className="text-[10px] text-[var(--color-text2)]">scope: {activeApproval.scope}</span>
                      )}
                    </div>
                    <div className="mt-2 text-[10px] uppercase text-[var(--color-text2)]">Redacted input</div>
                    <pre className="mt-1 max-h-24 overflow-auto whitespace-pre-wrap break-all font-mono text-xs text-[var(--color-text)]">
                      {activeApproval.redactedInput}
                    </pre>
                    {activeApproval.expiresAt && Number.isFinite(Date.parse(activeApproval.expiresAt)) && (
                      <div className="mt-1 text-[10px] text-[var(--color-text2)]">
                        Expires {new Date(activeApproval.expiresAt).toLocaleTimeString()}
                      </div>
                    )}
                    <div className="mt-2 flex items-center gap-1.5 text-xs text-[var(--color-text2)]">
                      {activeApproval.state === 'submitting' && (
                        <><LoaderCircle aria-hidden="true" className="w-3.5 h-3.5 animate-spin motion-reduce:animate-none" /> Submitting decision</>
                      )}
                      {activeApproval.state === 'error' && (
                        <><CircleX aria-hidden="true" className="w-3.5 h-3.5 text-[var(--color-red)]" /> {activeApproval.error}</>
                      )}
                    </div>
                    <div className="mt-3 flex flex-col-reverse sm:flex-row sm:justify-end gap-2">
                        <button
                          ref={denyApprovalRef}
                          type="button"
                          onClick={() => decideApproval('deny')}
                          disabled={activeApproval.state === 'submitting'}
                          className="min-h-10 w-full sm:w-auto px-3 py-2 rounded-md border border-[var(--color-border)] bg-[var(--color-bg)] text-xs font-medium text-[var(--color-text)] hover:border-[var(--color-red)] focus:outline-none focus:ring-2 focus:ring-[var(--color-red)] disabled:opacity-50"
                        >
                          Deny
                        </button>
                        <button
                          type="button"
                          onClick={() => decideApproval('allow_once')}
                          disabled={activeApproval.state === 'submitting'}
                          className="min-h-10 w-full sm:w-auto px-3 py-2 rounded-md border border-[var(--color-green)] bg-[var(--color-bg)] text-xs font-semibold text-[var(--color-text)] hover:bg-[var(--color-green)]/10 focus:outline-none focus:ring-2 focus:ring-[var(--color-green)] disabled:opacity-50"
                        >
                          Allow once
                        </button>
                    </div>
                  </div>
                </div>
              </div>
            )}
            <div ref={bottomRef} />
          </div>

          {/* Input */}
          <div className="px-4 py-3 border-t border-[var(--color-border)] bg-[var(--color-surface)]">
            {/* Image preview */}
            {attachedImage && (
              <div className="w-full mb-2 flex items-center gap-2">
                <div className="bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg p-1 flex items-center gap-2">
                  <img src={`data:${attachedImage.mediaType};base64,${attachedImage.data}`} className="h-12 rounded" alt="attached" />
                  <button type="button" onClick={() => setAttachedImage(null)} aria-label="첨부 이미지 제거" title="첨부 이미지 제거" className="text-xs text-[var(--color-red)] px-1 flex items-center"><X aria-hidden="true" className="w-3 h-3" /></button>
                </div>
                <span className="text-xs text-[var(--color-text2)]">Image attached</span>
              </div>
            )}
            {planReady && !streaming && (
              <div className="w-full mb-2 flex flex-wrap items-center justify-between gap-2 px-3 py-2 rounded-lg bg-[var(--color-surface2)] border border-[var(--color-accent)]">
                <span className="text-xs text-[var(--color-text)]">이 제안은 대화에만 있습니다. 버튼은 일반 요청을 보내며 저장된 Workstream Plan 승인을 기록하지 않습니다.</span>
                <button
                  onClick={() => send('위 계획대로 구현을 진행해줘.')}
                  className="text-xs font-semibold px-3 py-1.5 rounded-md bg-[var(--color-accent)] text-[var(--color-bg)] hover:opacity-90 whitespace-nowrap"
                >
                  이 계획으로 계속 요청
                </button>
              </div>
            )}
            {onWorkspaceModeChange && (
              <div className="w-full mb-2 flex items-center">
                <WorkspaceModeSwitch mode="single" onChange={onWorkspaceModeChange} attention={workspaceModeAttention} />
              </div>
            )}
            <div className="flex gap-2 items-end w-full">
              {/* Image upload */}
              <label className="px-3 py-3 rounded-xl cursor-pointer text-[var(--color-text2)] hover:bg-[var(--color-surface2)] transition-colors" title="Attach image" aria-label="이미지 첨부">
                <Paperclip aria-hidden="true" className="w-4 h-4" />
                <input
                  type="file"
                  accept="image/png,image/jpeg,image/gif,image/webp"
                  className="hidden"
                  onChange={async (e) => {
                    const file = e.target.files?.[0];
                    if (!file) return;
                    e.target.value = '';
                    try {
                      setAttachedImage(await readImageAttachment(file));
                      setSessionNotice('');
                    } catch (error) {
                      setSessionNotice(error instanceof Error ? error.message : 'Image could not be read.');
                    }
                  }}
                />
              </label>

              {/* Voice input */}
              <button
                onClick={() => {
                  if (!('webkitSpeechRecognition' in window || 'SpeechRecognition' in window)) {
                    alert('Speech recognition not supported in this browser');
                    return;
                  }
                  const speechWindow = window as Window & {
                    SpeechRecognition?: SpeechRecognitionConstructor;
                    webkitSpeechRecognition?: SpeechRecognitionConstructor;
                  };
                  const SpeechRecognition = speechWindow.SpeechRecognition || speechWindow.webkitSpeechRecognition;
                  if (!SpeechRecognition) return;
                  const recognition = new SpeechRecognition();
                  recognition.continuous = false;
                  recognition.interimResults = false;
                  recognition.lang = 'ko-KR';
                  recognition.onstart = () => setIsListening(true);
                  recognition.onend = () => setIsListening(false);
                  recognition.onresult = (event) => {
                    const text = event.results[0][0].transcript;
                    setInput((prev) => prev + text);
                  };
                  if (isListening) {
                    recognition.stop();
                  } else {
                    recognition.start();
                  }
                }}
                className={`px-3 py-3 rounded-xl transition-colors ${
                  isListening
                    ? 'bg-[var(--color-red)] text-[var(--color-bg)] animate-pulse'
                    : 'text-[var(--color-text2)] hover:bg-[var(--color-surface2)]'
                }`}
                title="Voice input"
                aria-label="음성 입력"
                aria-pressed={isListening}
              >
                <Mic aria-hidden="true" className="w-4 h-4" />
              </button>

              <textarea
                ref={inputRef}
                value={input}
                onChange={(e) => setInput(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send(); }
                  else if (e.key === 'Escape' && streaming) { e.preventDefault(); abortRef.current?.abort(); }
                }}
                onPaste={(e) => {
                  const items = e.clipboardData?.items;
                  if (!items) return;
                  for (const item of Array.from(items)) {
                    if (item.type.startsWith('image/')) {
                      e.preventDefault();
                      const file = item.getAsFile();
                      if (!file) return;
                      void readImageAttachment(file).then((attached) => {
                        setAttachedImage(attached);
                        setSessionNotice('');
                      }).catch((error: unknown) => {
                        setSessionNotice(error instanceof Error ? error.message : 'Image could not be read.');
                      });
                    }
                  }
                }}
                placeholder={t('chat.placeholder')}
                rows={1}
                className="flex-1 bg-[var(--color-bg)] border border-[var(--color-border)] rounded-xl px-4 py-3 text-sm text-[var(--color-text)] resize-none focus:outline-none focus:border-[var(--color-accent)] placeholder:text-[var(--color-text2)]"
                style={{ minHeight: '44px', maxHeight: '200px' }}
                onInput={(e) => {
                  const el = e.currentTarget;
                  el.style.height = 'auto';
                  el.style.height = Math.min(el.scrollHeight, 200) + 'px';
                }}
              />

              {/* TTS for last response */}
              <button
                onClick={() => {
                  const lastAssistant = messages.filter(m => m.role === 'assistant').pop();
                  if (!lastAssistant?.content) return;
                  const utterance = new SpeechSynthesisUtterance(lastAssistant.content.slice(0, 500));
                  utterance.lang = 'ko-KR';
                  speechSynthesis.speak(utterance);
                }}
                className="px-3 py-3 rounded-xl text-[var(--color-text2)] hover:bg-[var(--color-surface2)] transition-colors"
                title="Read last response aloud"
                aria-label="마지막 응답 읽어 주기"
              >
                <Volume2 aria-hidden="true" className="w-4 h-4" />
              </button>

              {streaming ? (
                <button
                  onClick={() => abortRef.current?.abort()}
                  className="px-5 py-3 bg-[var(--color-red)] text-[var(--color-bg)] rounded-xl text-sm font-medium hover:opacity-90 transition-colors flex items-center gap-1.5"
                  title="Stop generation (Esc)"
                >
                  <span aria-hidden="true" className="w-2.5 h-2.5 bg-[var(--color-bg)] rounded-[2px]" />
                  Stop
                </button>
              ) : (
                <button
                  onClick={() => send()}
                  disabled={!input.trim() && !attachedImage}
                  className="px-5 py-3 bg-[var(--color-accent)] text-[var(--color-bg)] rounded-xl text-sm font-medium disabled:opacity-40 hover:bg-[var(--color-accent2)] transition-colors"
                >
                  {t('chat.send')}
                </button>
              )}
            </div>
          </div>
        </div>

        {/* Right-side Inspector Drawer */}
        <Inspector
          open={inspectorOpen}
          onClose={() => setInspectorOpen(false)}
          tab={inspectorTab}
          onTabChange={setInspectorTab}
          changedFiles={changedFiles}
          diffRecords={diffRecords}
          currentTurnDiffs={currentTurnDiffs}
          undoEntries={undoEntries}
          undoActionBusy={undoActionBusy}
          onCheckRestore={handleCheckRestore}
          onRestoreEntry={handleRestoreUndoEntry}
          workstreams={workstreams}
          selectedWorkstreamId={selectedWorkstreamId}
          onSelectWorkstream={changeWorkstream}
          onCreateWorkstream={createCurrentWorkstream}
          onExportHandoff={exportHandoff}
          cannotClearWorkstream={cannotClearWorkstream}
          plans={plans}
          selectedPlanId={selectedPlanId}
          selectedPlan={selectedPlan}
          selectedPlanRevision={selectedPlanRevision}
          plansLoading={plansLoading}
          cannotClearPlan={cannotClearPlan}
          onSelectPlan={changePlan}
          selectedStageId={selectedStageId}
          selectedStage={selectedStage}
          onSelectStage={changeStage}
          planActionBusy={planActionBusy}
          onBindCurrentPlanRevision={bindCurrentPlanRevision}
          onApproveSelectedPlan={approveSelectedPlan}
          onReconcileSelectedPlanStage={reconcileSelectedPlanStage}
          currentSession={currentSession}
          sessionNotice={sessionNotice}
          workstreamNotice={workstreamNotice}
          latestReceipt={latestReceipt}
          streaming={streaming}
          onNavigateHistory={(historyTab) => onOpenHistory?.(historyTab)}
        />
      </div>
    </div>
  );
}
