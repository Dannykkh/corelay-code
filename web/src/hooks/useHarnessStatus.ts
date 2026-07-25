
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

export type HarnessLoadStatus = "idle" | "loading" | "success" | "error";

export interface SlashCommandInfo {
  name: string;
  description?: string;
  skillName?: string;
  skillPath?: string;
}

export interface AgentTypeInfo {
  name: string;
  description: string;
  tools?: string[];
  readOnly?: boolean;
  model?: string;
}

export interface SkillInfo {
  name: string;
  path: string;
  source: string;
}

export interface AgentLoopSnapshot {
  sessionId?: string;
  id?: string;
  status?: string;
  task?: string;
  workDir?: string;
  startedAt?: string;
  [key: string]: unknown;
}

export interface WorkstreamSummary {
  id: string;
  title?: string;
  status?: string;
  nextAction?: string;
  summary?: string;
  [key: string]: unknown;
}

export interface HarnessStatus {
  commands: SlashCommandInfo[];
  agentTypes: Record<string, AgentTypeInfo>;
  skills: SkillInfo[];
  loops: {
    loops: AgentLoopSnapshot[];
    maxConcurrent: number;
    count: number;
  };
  workstreams: {
    workDir: string;
    workstreams: WorkstreamSummary[];
  };
  plan: Record<string, unknown>;
}

export interface HarnessStatusState {
  status: HarnessLoadStatus;
  data: HarnessStatus | null;
  error: string | null;
  refresh: () => Promise<void>;
}

const DEFAULT_REFRESH_MS = 20_000;
const REQUEST_TIMEOUT_MS = 6_000;

function normalizeApiUrl(apiUrl: string) {
  return apiUrl.trim().replace(/\/+$/, "");
}

async function fetchJson<T>(baseUrl: string, path: string, signal: AbortSignal): Promise<T> {
  const response = await fetch(`${baseUrl}${path}`, {
    cache: "no-store",
    signal,
  });
  if (!response.ok) {
    throw new Error(`${path} returned ${response.status}`);
  }
  return response.json() as Promise<T>;
}

export function useHarnessStatus(apiUrl: string, refreshMs = DEFAULT_REFRESH_MS): HarnessStatusState {
  const baseUrl = useMemo(() => normalizeApiUrl(apiUrl), [apiUrl]);
  const abortRef = useRef<AbortController | null>(null);
  const [state, setState] = useState<Omit<HarnessStatusState, "refresh">>({
    status: "idle",
    data: null,
    error: null,
  });

  const refresh = useCallback(async () => {
    if (!baseUrl) {
      setState((current) => ({
        ...current,
        status: "error",
        error: "API base URL is empty",
      }));
      return;
    }

    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    const timeout = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS);

    setState((current) => ({
      ...current,
      status: current.data ? "success" : "loading",
      error: null,
    }));

    try {
      const [commands, agentTypes, skills, loops, workstreams, plan] = await Promise.all([
        fetchJson<SlashCommandInfo[]>(baseUrl, "/api/commands", controller.signal),
        fetchJson<Record<string, AgentTypeInfo>>(baseUrl, "/api/agent-types", controller.signal),
        fetchJson<SkillInfo[]>(baseUrl, "/api/skills", controller.signal),
        fetchJson<HarnessStatus["loops"]>(baseUrl, "/api/agent/loops", controller.signal),
        fetchJson<HarnessStatus["workstreams"]>(baseUrl, "/api/workstreams", controller.signal),
        fetchJson<Record<string, unknown>>(baseUrl, "/api/plan", controller.signal),
      ]);
      setState({
        status: "success",
        data: { commands, agentTypes, skills, loops, workstreams, plan },
        error: null,
      });
    } catch (error) {
      if (controller.signal.aborted) return;
      setState((current) => ({
        status: "error",
        data: current.data,
        error: error instanceof Error ? error.message : "Harness API unavailable",
      }));
    } finally {
      clearTimeout(timeout);
      if (abortRef.current === controller) {
        abortRef.current = null;
      }
    }
  }, [baseUrl]);

  useEffect(() => {
    refresh();
    const timer = setInterval(refresh, refreshMs);
    return () => {
      clearInterval(timer);
      abortRef.current?.abort();
    };
  }, [refresh, refreshMs]);

  return { ...state, refresh };
}
