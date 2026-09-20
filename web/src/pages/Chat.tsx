import { useState, useRef, useEffect, useCallback } from 'react';
import { t } from '../lib/i18n';
import { CircleCheck, CircleX, LoaderCircle, ShieldAlert } from 'lucide-react';
import { Markdown } from '../components/Markdown';
import { HTTPError, resolveApproval, type ApprovalDecision } from '../lib/api';
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
  toolInput?: Record<string, unknown> | string;
  toolResult?: string;
  toolDiff?: string;
  isError?: boolean;
  timestamp: Date;
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

export function ChatPage({ selectedWorkspace, loadSessionId, onSessionLoaded }: ChatPageProps) {
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
    const displayText = attachedImage ? `${text} [📎 image attached]` : text;
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
      const res = await fetch('/api/agent', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: apiMessages,
          workstreamId: runBinding.workstreamId || undefined,
          durableSessionId,
          expectedRevision,
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
      case 'undo_preview':
        setUndoEntries(readUndoEntries(data.entries));
        setUndoActionBusy(false);
        if (typeof data.error === 'string' && data.error) setSessionNotice(data.error);
        break;
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
          const updated = [...prev];
          for (let i = updated.length - 1; i >= 0; i--) {
            if (updated[i].role === 'tool' && updated[i].toolName === data.toolName && !updated[i].toolResult) {
              updated[i] = { ...updated[i], toolInput: undefined };
              break;
            }
          }
          return updated;
        });
        setActiveApproval({
          id: data.id,
          runtimeSessionId,
          toolName: data.toolName,
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
          toolInput: data.input, timestamp: new Date(),
        }]);
        break;
      case 'tool_result':
        setMessages((prev) => {
          const updated = [...prev];
          for (let i = updated.length - 1; i >= 0; i--) {
            if (updated[i].role === 'tool' && updated[i].toolName === data.name && !updated[i].toolResult) {
              updated[i] = { ...updated[i], toolResult: data.result, isError: data.isError };
              break;
            }
          }
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
        if (event.type === 'done' && completionBlocksSuccess(data)) {
          const status = data.completionStatus || data.terminalState || 'blocked';
          const revision = typeof data.completionRevision === 'number' ? ` at revision ${data.completionRevision}` : '';
          setSessionNotice(`Run ended without successful completion: ${status}${revision}. Continue or reconcile this session.`);
        } else if (event.type === 'done' && data.planMode) {
          setPlanReady(true);
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
  const liveLabel = streaming
    ? `${status || 'Thinking…'} · ${elapsed}s${genChars > 0 ? ` · ${genChars.toLocaleString()} chars` : ''}`
    : '';
  const selectedWorkstream = workstreams.find((w) => w.id === selectedWorkstreamId);
  const selectedPlan = plans.find((plan) => plan.id === selectedPlanId);
  const selectedStage = selectedPlan?.stages.find((stage) => stage.id === selectedStageId);
  const selectedStageName = selectedPlan?.definition.stages?.find((stage) => stage.id === selectedStageId)?.name || selectedStage?.id;
  const changedFiles = Array.from(new Set(diffRecords.map((record) => record.file).filter(Boolean)));
  const currentTurnDiffs = diffRecords.filter((record) => record.turn === turnNumberRef.current);
  const currentSessionLabel = currentSession
    ? `${currentSession.id.slice(0, 8)} · r${currentSession.revision} · ${currentSession.lifecycleStatus}`
    : 'new';
  const permissionLabel = executionPolicyPending
    ? 'checking…'
    : effectiveExecutionPolicy
      ? `${executionModeLabel(effectiveExecutionPolicy.mode)} · r${effectiveExecutionPolicy.revision}`
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
    if (!selectedWorkstreamId || !plan || !stage || stage.status !== 'running' || !attempt || streaming || planActionBusy) return;
    const confirmed = window.confirm(
      `Plan stage ${stage.id} is still recorded as running (${attempt.runId}). Confirm the old process is stopped and inspect its file changes before marking this attempt incomplete?`,
    );
    if (!confirmed) return;
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

  return (
    <div className="flex flex-col flex-1 min-w-0 h-full w-full">
      {/* Header */}
      <div className="px-4 py-2 border-b border-[var(--color-border)] bg-[var(--color-surface)] flex items-center justify-between">
        <div className="flex items-center gap-2">
          {approvalNeedsAction ? (
            <div className="flex items-center gap-1.5 text-[var(--color-yellow)] text-xs font-medium">
              <ShieldAlert aria-hidden="true" className="w-3.5 h-3.5" />
              Permission required
            </div>
          ) : streaming ? (
            <div className="flex items-center gap-1.5 text-[var(--color-accent)] text-xs">
              <div className="w-2 h-2 rounded-full bg-[var(--color-accent)] animate-pulse" />
              {liveLabel}
            </div>
          ) : status ? (
            <div className="flex items-center gap-1.5 text-[var(--color-accent)] text-xs">
              <div className="w-2 h-2 rounded-full bg-[var(--color-accent)] animate-pulse" />
              {status}
            </div>
          ) : (
            <span className="text-xs text-[var(--color-text2)]">
              {messages.length > 0 ? `${messages.filter(m => m.role === 'user').length} messages` : 'New conversation'}
            </span>
          )}
        </div>
        <div className="flex items-center gap-2 min-w-0">
          <select
            value={selectedWorkstreamId}
            onChange={(e) => changeWorkstream(e.target.value)}
            disabled={streaming || planActionBusy}
            className="max-w-72 bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg px-2 py-1.5 text-xs text-[var(--color-text)] focus:outline-none focus:border-[var(--color-accent)]"
            title="Workstream"
          >
            <option value="" disabled={cannotClearWorkstream}>No workstream</option>
            {workstreams.map((ws) => (
              <option key={ws.id} value={ws.id}>{ws.title}</option>
            ))}
          </select>
          <button
            onClick={createCurrentWorkstream}
            disabled={streaming || planActionBusy}
            className="px-2.5 py-1.5 text-xs rounded-lg border border-[var(--color-border)] text-[var(--color-text)] hover:border-[var(--color-accent)]"
          >
            New
          </button>
          <button
            onClick={exportHandoff}
            disabled={!selectedWorkstreamId}
            className="px-2.5 py-1.5 text-xs rounded-lg border border-[var(--color-border)] text-[var(--color-text)] disabled:opacity-40 hover:border-[var(--color-accent)]"
          >
            Handoff
          </button>
        </div>
      </div>

      <nav
        aria-label="Current project, workstream, session, stage, and permission"
        className="px-4 py-1.5 border-b border-[var(--color-border)] bg-[var(--color-bg)] flex flex-wrap items-center gap-x-2 gap-y-1 text-[10px] text-[var(--color-text2)]"
      >
        <span title={activeWorkspace || 'No project'}><span className="text-[var(--color-text)]">Project</span> {workspaceLabel(activeWorkspace)}</span>
        <span aria-hidden="true">›</span>
        <span title={selectedWorkstream?.id || selectedWorkstreamId || 'No workstream'}><span className="text-[var(--color-text)]">Work</span> {selectedWorkstream?.title || (selectedWorkstreamId ? 'unavailable' : 'none')}</span>
        <span aria-hidden="true">›</span>
        <span title={currentSession?.id || 'No durable session'}><span className="text-[var(--color-text)]">Session</span> {currentSessionLabel}</span>
        <span aria-hidden="true">›</span>
        <span title={selectedPlan?.id || selectedPlanId || 'No plan'}><span className="text-[var(--color-text)]">Stage</span> {selectedStageName ? `${selectedStageName} · ${selectedStage?.status || 'unknown'}` : 'none'}</span>
        <span aria-hidden="true">›</span>
        <span title={effectiveExecutionPolicy ? `Effective execution mode, revision ${effectiveExecutionPolicy.revision}` : 'Effective execution mode has not been confirmed'}><span className="text-[var(--color-text)]">Permission</span> {permissionLabel}</span>
      </nav>

      {selectedWorkstreamId && (
        <div className="px-4 py-2 border-b border-[var(--color-border)] bg-[var(--color-bg)] flex flex-wrap items-center gap-2 text-xs">
          <label className="flex items-center gap-1.5 text-[var(--color-text2)]">
            Plan
            <select
              value={selectedPlanId}
              onChange={(e) => changePlan(e.target.value)}
              disabled={streaming || planActionBusy || plansLoading}
              className="max-w-64 bg-[var(--color-surface)] border border-[var(--color-border)] rounded-lg px-2 py-1.5 text-xs text-[var(--color-text)] focus:outline-none focus:border-[var(--color-accent)] disabled:opacity-50"
              aria-label="Workstream plan"
            >
              <option value="" disabled={cannotClearPlan}>Chat only</option>
              {plans.map((plan) => (
                <option key={plan.id} value={plan.id}>
                  {plan.definition.name || plan.definition.objective || plan.id} · {plan.status} · r{plan.revision}
                </option>
              ))}
            </select>
          </label>
          <label className="flex items-center gap-1.5 text-[var(--color-text2)]">
            Stage
            <select
              value={selectedStageId}
              onChange={(e) => changeStage(e.target.value)}
              disabled={streaming || planActionBusy || !selectedPlan || selectedPlan.stages.length === 0}
              className="max-w-56 bg-[var(--color-surface)] border border-[var(--color-border)] rounded-lg px-2 py-1.5 text-xs text-[var(--color-text)] focus:outline-none focus:border-[var(--color-accent)] disabled:opacity-50"
              aria-label="Plan stage"
            >
              {!selectedStageId && <option value="">Select stage</option>}
              {selectedPlan && selectedPlan.stages.map((stage) => (
                <option key={stage.id} value={stage.id} disabled={!planStageEligible(selectedPlan, stage.id)}>
                  {selectedPlan.definition.stages?.find((item) => item.id === stage.id)?.name || stage.id} · {stage.status}
                </option>
              ))}
            </select>
          </label>
          {plansLoading && <span className="text-[var(--color-text2)]">Plans loading…</span>}
          {!plansLoading && selectedPlanId && !selectedPlan && (
            <span className="text-[var(--color-red)]">선택한 Plan을 불러올 수 없습니다.</span>
          )}
          {selectedPlan && (
            <>
              <span className={selectedPlan.approvedRevision === selectedPlan.revision ? 'text-[var(--color-green)]' : 'text-[var(--color-yellow)]'}>
                {selectedPlan.approvedRevision === selectedPlan.revision
                  ? `승인됨 · r${selectedPlan.revision}`
                  : `승인 필요 · ${selectedPlan.status} · r${selectedPlan.revision}`}
              </span>
              <span className="text-[var(--color-text2)]">
                {selectedPlan.stages.filter((stage) => stage.status === 'completed').length}/{selectedPlan.stages.length} stages complete
              </span>
              {selectedPlanRevision !== selectedPlan.revision && (
                <button
                  onClick={bindCurrentPlanRevision}
                  disabled={streaming || planActionBusy}
                  className="px-2.5 py-1.5 rounded-lg border border-[var(--color-border)] text-[var(--color-text)] hover:border-[var(--color-accent)] disabled:opacity-50"
                >
                  현재 revision 연결
                </button>
              )}
              <button
                onClick={approveSelectedPlan}
                disabled={streaming || planActionBusy || selectedPlan.status !== 'draft' ||
                  selectedPlanRevision !== selectedPlan.revision || !selectedPlan.revision || !selectedPlan.stateRevision}
                className="px-2.5 py-1.5 rounded-lg border border-[var(--color-border)] text-[var(--color-text)] hover:border-[var(--color-accent)] disabled:opacity-40"
                title={selectedPlan.status !== 'draft' ? 'Draft Plan만 승인할 수 있습니다.' : '현재 definition/state revision을 CAS로 승인합니다.'}
              >
                {planActionBusy ? '승인 중…' : 'Plan 승인'}
              </button>
              {selectedPlan.stages.find((stage) => stage.id === selectedStageId)?.status === 'running' && (
                <button
                  onClick={reconcileSelectedPlanStage}
                  disabled={streaming || planActionBusy}
                  className="px-2.5 py-1.5 rounded-lg border border-[var(--color-yellow)] text-[var(--color-yellow)] hover:border-[var(--color-accent)] disabled:opacity-40"
                  title="실행 프로세스가 끝났음을 확인한 뒤 미완료 상태로 수동 복구합니다."
                >
                  {planActionBusy ? '상태 처리 중…' : '중단 단계 복구'}
                </button>
              )}
            </>
          )}
        </div>
      )}

      {(selectedWorkstream || workstreamNotice || sessionNotice) && (
        <div className="px-4 py-2 border-b border-[var(--color-border)] bg-[var(--color-bg)] flex items-center gap-3 text-xs min-h-10">
          {selectedWorkstream && (
            <>
              <span className="font-medium text-[var(--color-text)] truncate max-w-64">{selectedWorkstream.title}</span>
              <span className="text-[var(--color-text2)]">{selectedWorkstream.status}</span>
              {selectedWorkstream.lastVerification?.status && (
                <span className="text-[var(--color-text2)]">verify: {selectedWorkstream.lastVerification.status}</span>
              )}
              {selectedWorkstream.nextAction && (
                <span className="text-[var(--color-text2)] truncate min-w-0">next: {selectedWorkstream.nextAction}</span>
              )}
            </>
          )}
          {workstreamNotice && (
            <span className="ml-auto text-[var(--color-text2)] truncate">{workstreamNotice}</span>
          )}
          {sessionNotice && (
            <span className="ml-auto text-[var(--color-text2)] truncate">{sessionNotice}</span>
          )}
        </div>
      )}

      {(diffRecords.length > 0 || undoEntries.length > 0) && (
        <section className="px-4 py-2 border-b border-[var(--color-border)] bg-[var(--color-surface)] text-xs" aria-label="Run changes and restore">
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-medium text-[var(--color-text)]">Changes</span>
            <span className="text-[var(--color-text2)]">{changedFiles.length} file{changedFiles.length === 1 ? '' : 's'} · {diffRecords.length} diff{diffRecords.length === 1 ? '' : 's'}</span>
            {currentTurnDiffs.length > 0 && <span className="text-[var(--color-accent)]">current turn {currentTurnDiffs.length}</span>}
            <button
              onClick={() => { setUndoActionBusy(true); void send('/undo --list'); }}
              disabled={streaming || undoActionBusy}
              className="ml-auto px-2 py-1 rounded border border-[var(--color-border)] text-[var(--color-text)] hover:border-[var(--color-accent)] disabled:opacity-50"
            >
              {undoActionBusy ? 'Checking restore…' : 'Check restore'}
            </button>
          </div>
          {changedFiles.length > 0 && (
            <div className="mt-1 text-[var(--color-text2)] truncate" title={changedFiles.join(', ')}>
              {changedFiles.join(' · ')}
            </div>
          )}
          {undoEntries.length > 0 && (
            <div className="mt-2 flex flex-col gap-1.5">
              <span className="text-[var(--color-text2)]">Checkpoint restore candidates</span>
              {undoEntries.map((entry) => {
                const restorable = entry.status === 'restorable';
                return (
                  <div key={entry.id} className="flex items-center gap-2">
                    <span className={`truncate flex-1 ${restorable ? 'text-[var(--color-text)]' : 'text-[var(--color-yellow)]'}`} title={entry.path}>
                      {entry.path} · {entry.status}
                    </span>
                    {restorable && (
                      <button
                        onClick={() => { setUndoActionBusy(true); void send(`/undo --select ${entry.id}`); }}
                        disabled={streaming || undoActionBusy}
                        className="px-2 py-0.5 rounded border border-[var(--color-border)] text-[var(--color-text)] hover:border-[var(--color-accent)] disabled:opacity-50"
                      >
                        Restore
                      </button>
                    )}
                  </div>
                );
              })}
            </div>
          )}
          {diffRecords.length > 0 && (
            <details className="mt-2">
              <summary className="cursor-pointer text-[var(--color-text2)]">Show accumulated turn diffs</summary>
              <div className="mt-1 max-h-56 overflow-auto space-y-2">
                {diffRecords.map((record, index) => (
                  <div key={`${record.turn}-${record.file}-${index}`} className="border-l-2 border-[var(--color-border)] pl-2">
                    <div className="text-[var(--color-text)]">turn {record.turn} · {record.file}</div>
                    <pre className="mt-0.5 whitespace-pre-wrap text-[10px] font-mono text-[var(--color-text2)]">{record.diff}</pre>
                  </div>
                ))}
              </div>
            </details>
          )}
        </section>
      )}

      {/* Messages Area — full width */}
      <div className="flex-1 overflow-y-auto px-6 py-4 space-y-3">
            {messages.length === 0 && (
              <div className="flex flex-col items-center justify-center h-full text-[var(--color-text2)] max-w-lg mx-auto">
                <div className="w-14 h-14 rounded-2xl bg-gradient-to-br from-[var(--color-accent)] to-purple-400 flex items-center justify-center text-white text-xl font-bold mb-5">C</div>
                <div className="text-xl font-semibold mb-2 text-[var(--color-text)]">What can I help you with?</div>
                <div className="text-sm mb-8 text-center">I can read your code, write new files, run commands, search your project, and more.</div>

                <div className="grid grid-cols-2 gap-2.5 w-full">
                  {[
                    { text: 'What does this project do?', desc: 'Understand the codebase' },
                    { text: 'Find bugs and fix them', desc: 'Debug and repair' },
                    { text: 'Add a new feature', desc: 'Write code for me' },
                    { text: 'Clean up this code', desc: 'Improve quality' },
                  ].map((example) => (
                    <button
                      key={example.text}
                      onClick={() => { setInput(example.text); }}
                      className="text-left px-4 py-3 bg-[var(--color-surface)] border border-[var(--color-border)] rounded-xl hover:border-[var(--color-accent)] transition-colors"
                    >
                      <div className="text-sm text-[var(--color-text)]">{example.text}</div>
                      <div className="text-[10px] text-[var(--color-text2)] mt-0.5">{example.desc}</div>
                    </button>
                  ))}
                </div>

                <div className="text-[10px] mt-6 text-center leading-relaxed">
                  Tip: Just type what you need in plain language. I'll figure out which files to read and what to do.
                </div>
              </div>
            )}

            {messages.map((msg, i) => {
              if (msg.role === 'user') {
                return (
                  <div key={i} className="flex justify-end">
                    <div className="max-w-[85%] bg-[var(--color-accent)] text-white rounded-xl rounded-br-sm px-4 py-3 text-sm">
                      <div className="whitespace-pre-wrap">{msg.content}</div>
                      <div className="text-[10px] text-white/50 mt-1">{msg.timestamp.toLocaleTimeString()}</div>
                    </div>
                  </div>
                );
              }
              if (msg.role === 'tool') {
                return (
                  <div key={i} className="mx-4">
                    <div className="bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg overflow-hidden">
                      <div className="flex items-center gap-2 px-3 py-2 bg-[var(--color-surface2)] border-b border-[var(--color-border)]">
                        <span className="text-xs font-semibold text-[var(--color-accent)]">{msg.toolName ?? ''}</span>
                        {msg.isError && <span className="text-[10px] text-[var(--color-red)] bg-red-500/10 px-1.5 py-0.5 rounded">ERROR</span>}
                      </div>
                      {msg.toolInput && (
                        <div className="px-3 py-2 border-b border-[var(--color-border)]">
                          <div className="text-[10px] text-[var(--color-text2)] uppercase mb-1">{t('chat.input')}</div>
                          <pre className="text-xs text-[var(--color-text)] overflow-x-auto whitespace-pre-wrap font-mono">
                            {typeof msg.toolInput === 'string' ? msg.toolInput : String(JSON.stringify(msg.toolInput, null, 2))}
                          </pre>
                        </div>
                      )}
                      {msg.toolResult ? (
                        <div className="px-3 py-2 max-h-60 overflow-y-auto">
                          <div className="text-[10px] text-[var(--color-text2)] uppercase mb-1">{t('chat.output')}</div>
                          <pre className={`text-xs overflow-x-auto whitespace-pre-wrap font-mono ${msg.isError ? 'text-[var(--color-red)]' : 'text-[var(--color-green)]'}`}>
                            {msg.toolResult}
                          </pre>
                        </div>
                      ) : (
                        <div className="px-3 py-2 flex items-center gap-2">
                          <div className="w-3 h-3 border-2 border-[var(--color-accent)] border-t-transparent rounded-full animate-spin" />
                          <span className="text-xs text-[var(--color-text2)]">{t('chat.executing')}</span>
                        </div>
                      )}
                      {msg.toolDiff && (
                        <div className="px-3 py-2 border-t border-[var(--color-border)] max-h-72 overflow-auto">
                          <div className="text-[10px] text-[var(--color-text2)] uppercase mb-1">Diff</div>
                          <pre className="text-xs whitespace-pre font-mono leading-snug">
                            {msg.toolDiff.split('\n').map((ln, k) => {
                              const cls = ln.startsWith('+ ')
                                ? 'text-[var(--color-green)] bg-green-500/10'
                                : ln.startsWith('- ')
                                  ? 'text-[var(--color-red)] bg-red-500/10'
                                  : 'text-[var(--color-text2)]';
                              return <div key={k} className={cls}>{ln || ' '}</div>;
                            })}
                          </pre>
                        </div>
                      )}
                    </div>
                  </div>
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
                  </div>
                </div>
              );
            })}
            {activeApproval && (
              <div
                role="alert"
                aria-live="assertive"
                aria-busy={activeApproval.state === 'submitting'}
                className="mx-0 sm:mx-4 border border-[var(--color-yellow)] bg-[var(--color-surface2)] rounded-lg px-3 py-3"
              >
                <div className="flex items-start gap-2.5">
                  <ShieldAlert aria-hidden="true" className="w-4 h-4 mt-0.5 shrink-0 text-[var(--color-yellow)]" />
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                      <span className="text-xs font-semibold text-[var(--color-text)]">Permission required</span>
                      <span className="text-xs font-mono text-[var(--color-text)] break-all">{activeApproval.toolName}</span>
                      {activeApproval.dangerLevel && (
                        <span className="text-[10px] uppercase text-[var(--color-yellow)]">risk: {activeApproval.dangerLevel}</span>
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
                      {activeApproval.state === 'resolved' && activeApproval.decision === 'allow_once' && (
                        <><CircleCheck aria-hidden="true" className="w-3.5 h-3.5 text-[var(--color-green)]" /> Allowed once</>
                      )}
                      {activeApproval.state === 'resolved' && activeApproval.decision === 'deny' && (
                        <><CircleX aria-hidden="true" className="w-3.5 h-3.5 text-[var(--color-red)]" /> Denied</>
                      )}
                      {activeApproval.state === 'expired' && (
                        <><CircleX aria-hidden="true" className="w-3.5 h-3.5 text-[var(--color-red)]" /> Expired; nothing was approved</>
                      )}
                      {activeApproval.state === 'error' && (
                        <><CircleX aria-hidden="true" className="w-3.5 h-3.5 text-[var(--color-red)]" /> {activeApproval.error}</>
                      )}
                    </div>
                    {(activeApproval.state === 'pending' || activeApproval.state === 'submitting' || activeApproval.state === 'error') && (
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
                          className="min-h-10 w-full sm:w-auto px-3 py-2 rounded-md bg-[var(--color-accent)] text-xs font-semibold text-white hover:bg-[var(--color-accent2)] focus:outline-none focus:ring-2 focus:ring-[var(--color-accent)] disabled:opacity-50"
                        >
                          Allow once
                        </button>
                      </div>
                    )}
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
                  <button onClick={() => setAttachedImage(null)} className="text-xs text-[var(--color-red)] px-1">✕</button>
                </div>
                <span className="text-xs text-[var(--color-text2)]">Image attached</span>
              </div>
            )}
            {planReady && !streaming && (
              <div className="w-full mb-2 flex flex-wrap items-center justify-between gap-2 px-3 py-2 rounded-lg bg-[var(--color-surface2)] border border-[var(--color-accent)]">
                <span className="text-xs text-[var(--color-text)]">이 제안은 대화에만 있습니다. 버튼은 일반 요청을 보내며 저장된 Workstream Plan 승인을 기록하지 않습니다.</span>
                <button
                  onClick={() => send('위 계획대로 구현을 진행해줘.')}
                  className="text-xs font-semibold px-3 py-1.5 rounded-md bg-[var(--color-accent)] text-white hover:opacity-90 whitespace-nowrap"
                >
                  이 계획으로 계속 요청
                </button>
              </div>
            )}
            <div className="flex gap-2 items-end w-full">
              {/* Image upload */}
              <label className="px-3 py-3 rounded-xl cursor-pointer text-[var(--color-text2)] hover:bg-[var(--color-surface2)] transition-colors" title="Attach image">
                <span>📎</span>
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
                    ? 'bg-[var(--color-red)] text-white animate-pulse'
                    : 'text-[var(--color-text2)] hover:bg-[var(--color-surface2)]'
                }`}
                title="Voice input"
              >
                🎤
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
              >
                🔊
              </button>

              {streaming ? (
                <button
                  onClick={() => abortRef.current?.abort()}
                  className="px-5 py-3 bg-[var(--color-red)] text-white rounded-xl text-sm font-medium hover:opacity-90 transition-colors flex items-center gap-1.5"
                  title="Stop generation (Esc)"
                >
                  <span className="w-2.5 h-2.5 bg-white rounded-[2px]" />
                  Stop
                </button>
              ) : (
                <button
                  onClick={() => send()}
                  disabled={!input.trim() && !attachedImage}
                  className="px-5 py-3 bg-[var(--color-accent)] text-white rounded-xl text-sm font-medium disabled:opacity-40 hover:bg-[var(--color-accent2)] transition-colors"
                >
                  {t('chat.send')}
                </button>
              )}
            </div>
          </div>
    </div>
  );
}
