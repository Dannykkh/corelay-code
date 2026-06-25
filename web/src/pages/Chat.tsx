import { useState, useRef, useEffect, useCallback } from 'react';
import { t } from '../lib/i18n';
import { Markdown } from '../components/Markdown';
import { listSessions, getSession, saveSession, deleteSession, type SessionSummary, type SessionMessage } from '../lib/sessions';
import { createWorkstream, generateHandoff, listWorkstreams, type Workstream } from '../lib/workstreams';

interface ChatMessage {
  role: 'user' | 'assistant' | 'tool';
  content: string;
  toolName?: string;
  toolInput?: Record<string, unknown> | string;
  toolResult?: string;
  toolDiff?: string;
  isError?: boolean;
  timestamp: Date;
}

interface ChatPageProps {
  loadSessionId?: string | null;
  onSessionLoaded?: () => void;
}

export function ChatPage({ loadSessionId, onSessionLoaded }: ChatPageProps) {
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState('');
  const [streaming, setStreaming] = useState(false);
  const [status, setStatus] = useState('');
  const [planReady, setPlanReady] = useState(false); // a /plan run finished; offer Approve & Run
  const [attachedImage, setAttachedImage] = useState<string | null>(null); // base64
  const [isListening, setIsListening] = useState(false);
  const [sessionId, setSessionId] = useState<string | null>(null);
  const [_sessions, setSessions] = useState<SessionSummary[]>([]);
  const [workstreams, setWorkstreams] = useState<Workstream[]>([]);
  const [selectedWorkstreamId, setSelectedWorkstreamId] = useState('');
  const [workstreamNotice, setWorkstreamNotice] = useState('');
  // Liveness indicators for slow local models: elapsed seconds tick client-side
  // from the moment we send; genChars is the authoritative output size from the
  // backend heartbeat. Together they prove the model is alive, not hung —
  // modeled on Claude Code's "Thinking… (Ns · ↑N)" status line.
  const [elapsed, setElapsed] = useState(0);
  const [genChars, setGenChars] = useState(0);
  const tickRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  const bottomRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLTextAreaElement>(null);

  // Load session list
  useEffect(() => {
    listSessions().then((s) => setSessions(s || [])).catch(() => setSessions([]));
  }, []);

  const refreshWorkstreams = useCallback(async () => {
    try {
      const next = await listWorkstreams();
      setWorkstreams(next);
      setSelectedWorkstreamId((current) => {
        if (!current) return current;
        return next.some((w) => w.id === current) ? current : '';
      });
    } catch (err) {
      setWorkstreamNotice(err instanceof Error ? err.message : String(err));
    }
  }, []);

  useEffect(() => {
    refreshWorkstreams();
  }, [refreshWorkstreams]);

  // Handle session load/new from SidePanel
  useEffect(() => {
    if (!loadSessionId) return;
    if (loadSessionId === '__new__') {
      newChat();
    } else {
      loadSession(loadSessionId);
    }
    onSessionLoaded?.();
  }, [loadSessionId]);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, [messages, status]);

  // Stop the liveness clock if the component unmounts mid-stream.
  useEffect(() => () => { if (tickRef.current) clearInterval(tickRef.current); }, []);

  // Auto-save session after each message
  const autoSave = useCallback(async (msgs: ChatMessage[], sid: string | null) => {
    if (msgs.length === 0) return;
    const sessionMsgs: SessionMessage[] = msgs.map((m) => ({
      role: m.role,
      content: m.content,
      toolName: m.toolName,
      toolInput: m.toolInput,
      toolResult: m.toolResult,
      isError: m.isError,
      timestamp: m.timestamp.toISOString(),
    }));
    const result = await saveSession({ id: sid || undefined, messages: sessionMsgs });
    if (result.id && !sid) {
      setSessionId(result.id);
    }
    listSessions().then((s) => setSessions(s || [])).catch(() => setSessions([]));
  }, []);

  const loadSession = async (id: string) => {
    const sess = await getSession(id);
    const msgs: ChatMessage[] = (sess.messages || []).map((m) => ({
      ...m,
      timestamp: new Date(m.timestamp),
    }));
    setMessages(msgs);
    setSessionId(sess.id);
  };

  function newChat() {
    setMessages([]);
    setSessionId(null);
  }

  // @ts-expect-error reserved for SidePanel session list
  const _handleDelete = async (id: string) => {
    await deleteSession(id);
    setSessions((prev) => prev.filter((s) => s.id !== id));
    if (sessionId === id) {
      newChat();
    }
  };

  async function send(overrideText?: string) {
    const text = (overrideText ?? input).trim();
    if (!text && !attachedImage) return;
    if (streaming) return;

    setPlanReady(false); // any new turn clears the plan-approval prompt
    if (overrideText === undefined) setInput('');
    const displayText = attachedImage ? `${text} [📎 image attached]` : text;
    const userMsg: ChatMessage = { role: 'user', content: displayText || '[image]', timestamp: new Date() };
    const newMsgs = [...messages, userMsg];
    setMessages([...newMsgs, { role: 'assistant', content: '', timestamp: new Date() }]);
    setStreaming(true);
    setStatus(t('chat.thinking'));
    // Start the liveness clock immediately (don't wait for the first backend
    // heartbeat at t=1s, and cover the connecting phase before any token).
    setElapsed(0);
    setGenChars(0);
    if (tickRef.current) clearInterval(tickRef.current);
    tickRef.current = setInterval(() => setElapsed((e) => e + 1), 1000);

    // Build message content — include image if attached
    let msgContent: string = text || 'Analyze this image.';
    if (attachedImage) {
      msgContent = `[Image attached (base64, ${Math.round(attachedImage.length / 1024)}KB)]\n\n${msgContent}`;
    }

    const apiMessages = newMsgs
      .filter((m) => m.role === 'user' || m.role === 'assistant')
      .map((m) => ({ role: m.role, content: m.content }));
    // Replace last with the actual content including image reference
    if (apiMessages.length > 0) {
      apiMessages[apiMessages.length - 1] = { role: 'user', content: msgContent };
    }

    setAttachedImage(null); // clear after sending

    // Abort controller so the user can interrupt a slow generation; aborting
    // the fetch cancels the request context, which propagates to RunLoop and
    // stops the provider stream server-side.
    const controller = new AbortController();
    abortRef.current = controller;

    try {
      const res = await fetch('/api/agent', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          messages: apiMessages,
          workstreamId: selectedWorkstreamId || undefined,
        }),
        signal: controller.signal,
      });

      const reader = res.body!.getReader();
      const decoder = new TextDecoder();
      let buffer = '';

      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        const lines = buffer.split('\n');
        buffer = lines.pop() || '';
        for (const line of lines) {
          const trimmed = line.trim();
          if (!trimmed.startsWith('data: ')) continue;
          try {
            handleAgentEvent(JSON.parse(trimmed.slice(6)));
          } catch { /* skip */ }
        }
      }
    } catch (err) {
      const aborted = err instanceof DOMException && err.name === 'AbortError';
      setMessages((prev) => {
        const updated = [...prev];
        const last = updated[updated.length - 1];
        if (last?.role === 'assistant') {
          const note = aborted ? '\n\n_(stopped)_' : `\n\n[Error: ${err}]`;
          updated[updated.length - 1] = { ...last, content: last.content + note };
        }
        return updated;
      });
    } finally {
      setStreaming(false);
      setStatus('');
      abortRef.current = null;
      if (tickRef.current) { clearInterval(tickRef.current); tickRef.current = null; }
      inputRef.current?.focus();
      // Auto-save after response complete
      setMessages((prev) => {
        autoSave(prev, sessionId);
        return prev;
      });
    }
  }

  function handleAgentEvent(event: { type: string; data: any }) {
    switch (event.type) {
      case 'thinking':
        // Append thinking text to assistant message (wrapped in <think> tags for rendering)
        setMessages((prev) => {
          const updated = [...prev];
          const last = updated[updated.length - 1];
          if (last?.role === 'assistant') {
            const thinkTag = last.content.includes('<think>') ? '' : '<think>';
            updated[updated.length - 1] = { ...last, content: last.content + thinkTag + event.data };
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
            updated[updated.length - 1] = { ...last, content: last.content + closingTag + event.data };
          }
          return updated;
        });
        break;
      case 'tool_start':
        setStatus(`${t('chat.executing')} ${event.data.name}`);
        break;
      case 'tool_input':
        setMessages((prev) => [...prev, {
          role: 'tool' as const, content: '', toolName: event.data.name,
          toolInput: event.data.input, timestamp: new Date(),
        }]);
        break;
      case 'tool_result':
        setMessages((prev) => {
          const updated = [...prev];
          for (let i = updated.length - 1; i >= 0; i--) {
            if (updated[i].role === 'tool' && updated[i].toolName === event.data.name && !updated[i].toolResult) {
              updated[i] = { ...updated[i], toolResult: event.data.result, isError: event.data.isError };
              break;
            }
          }
          return updated;
        });
        setStatus('');
        setMessages((prev) => [...prev, { role: 'assistant', content: '', timestamp: new Date() }]);
        break;
      case 'diff':
        // Attach the before/after diff to the most recent Edit/Write tool card.
        setMessages((prev) => {
          const updated = [...prev];
          for (let i = updated.length - 1; i >= 0; i--) {
            if (updated[i].role === 'tool' && !updated[i].toolDiff &&
                (updated[i].toolName === 'Edit' || updated[i].toolName === 'Write')) {
              updated[i] = { ...updated[i], toolDiff: event.data.diff };
              break;
            }
          }
          return updated;
        });
        break;
      case 'heartbeat':
        // Authoritative output size + elapsed from the backend liveness ticker.
        if (typeof event.data?.chars === 'number') setGenChars(event.data.chars);
        if (typeof event.data?.elapsedMs === 'number') setElapsed(Math.floor(event.data.elapsedMs / 1000));
        break;
      case 'workstream':
        if (typeof event.data?.id === 'string') {
          setSelectedWorkstreamId(event.data.id);
        }
        break;
      case 'status':
        setStatus(event.data);
        break;
      case 'done':
      case 'stream_end':
        if (event.type === 'done' && event.data?.planMode) setPlanReady(true);
        setStatus('');
        setMessages((prev) => {
          if (prev[prev.length - 1]?.role === 'assistant' && prev[prev.length - 1]?.content === '') {
            return prev.slice(0, -1);
          }
          return prev;
        });
        refreshWorkstreams();
        break;
      case 'error':
        setMessages((prev) => [...prev, { role: 'assistant', content: `Error: ${event.data}`, timestamp: new Date() }]);
        break;
    }
  }

  // "Thinking… · 12s · 340 chars" — a moving clock + growing output size is the
  // canonical "it's alive" signal for a slow local model (Claude Code style).
  const liveLabel = streaming
    ? `${status || 'Thinking…'} · ${elapsed}s${genChars > 0 ? ` · ${genChars.toLocaleString()} chars` : ''}`
    : '';
  const selectedWorkstream = workstreams.find((w) => w.id === selectedWorkstreamId);

  async function createCurrentWorkstream() {
    const seed = input.trim() || messages.find((m) => m.role === 'user')?.content || 'Local agent workstream';
    const title = seed.split('\n')[0].slice(0, 80) || 'Local agent workstream';
    try {
      const created = await createWorkstream({
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
      setSelectedWorkstreamId(created.id);
      setWorkstreamNotice(`Created ${created.title}`);
      await refreshWorkstreams();
    } catch (err) {
      setWorkstreamNotice(err instanceof Error ? err.message : String(err));
    }
  }

  async function exportHandoff() {
    if (!selectedWorkstreamId) return;
    try {
      const result = await generateHandoff(selectedWorkstreamId);
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
          ) : (
            <span className="text-xs text-[var(--color-text2)]">
              {messages.length > 0 ? `${messages.filter(m => m.role === 'user').length} messages` : 'New conversation'}
            </span>
          )}
        </div>
        <div className="flex items-center gap-2 min-w-0">
          <select
            value={selectedWorkstreamId}
            onChange={(e) => setSelectedWorkstreamId(e.target.value)}
            className="max-w-72 bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg px-2 py-1.5 text-xs text-[var(--color-text)] focus:outline-none focus:border-[var(--color-accent)]"
            title="Workstream"
          >
            <option value="">No workstream</option>
            {workstreams.map((ws) => (
              <option key={ws.id} value={ws.id}>{ws.title}</option>
            ))}
          </select>
          <button
            onClick={createCurrentWorkstream}
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

      {(selectedWorkstream || workstreamNotice) && (
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
        </div>
      )}

      {/* Messages Area — full width */}
      <div className="flex-1 overflow-y-auto px-6 py-4 space-y-3">
            {messages.length === 0 && (
              <div className="flex flex-col items-center justify-center h-full text-[var(--color-text2)] max-w-lg mx-auto">
                <div className="w-14 h-14 rounded-2xl bg-gradient-to-br from-[var(--color-accent)] to-purple-400 flex items-center justify-center text-white text-xl font-bold mb-5">A</div>
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
            <div ref={bottomRef} />
          </div>

          {/* Input */}
          <div className="px-4 py-3 border-t border-[var(--color-border)] bg-[var(--color-surface)]">
            {/* Image preview */}
            {attachedImage && (
              <div className="w-full mb-2 flex items-center gap-2">
                <div className="bg-[var(--color-bg)] border border-[var(--color-border)] rounded-lg p-1 flex items-center gap-2">
                  <img src={`data:image/png;base64,${attachedImage}`} className="h-12 rounded" alt="attached" />
                  <button onClick={() => setAttachedImage(null)} className="text-xs text-[var(--color-red)] px-1">✕</button>
                </div>
                <span className="text-xs text-[var(--color-text2)]">Image attached</span>
              </div>
            )}
            {planReady && !streaming && (
              <div className="w-full mb-2 flex items-center justify-between gap-2 px-3 py-2 rounded-lg bg-[var(--color-surface2)] border border-[var(--color-accent)]">
                <span className="text-xs text-[var(--color-text)]">계획이 준비됐어요. 검토 후 진행하세요.</span>
                <button
                  onClick={() => send('위 계획대로 구현을 진행해줘.')}
                  className="text-xs font-semibold px-3 py-1.5 rounded-md bg-[var(--color-accent)] text-white hover:opacity-90 whitespace-nowrap"
                >
                  승인 &amp; 실행
                </button>
              </div>
            )}
            <div className="flex gap-2 items-end w-full">
              {/* Image upload */}
              <label className="px-3 py-3 rounded-xl cursor-pointer text-[var(--color-text2)] hover:bg-[var(--color-surface2)] transition-colors" title="Attach image">
                <span>📎</span>
                <input
                  type="file"
                  accept="image/*"
                  className="hidden"
                  onChange={async (e) => {
                    const file = e.target.files?.[0];
                    if (!file) return;
                    const reader = new FileReader();
                    reader.onload = () => {
                      const result = reader.result as string;
                      const b64 = result.split(',')[1];
                      setAttachedImage(b64);
                    };
                    reader.readAsDataURL(file);
                    e.target.value = '';
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
                  const SpeechRecognition = (window as any).SpeechRecognition || (window as any).webkitSpeechRecognition;
                  const recognition = new SpeechRecognition();
                  recognition.continuous = false;
                  recognition.interimResults = false;
                  recognition.lang = 'ko-KR';
                  recognition.onstart = () => setIsListening(true);
                  recognition.onend = () => setIsListening(false);
                  recognition.onresult = (event: any) => {
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
                      const reader = new FileReader();
                      reader.onload = () => {
                        const result = reader.result as string;
                        setAttachedImage(result.split(',')[1]);
                      };
                      reader.readAsDataURL(file);
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
