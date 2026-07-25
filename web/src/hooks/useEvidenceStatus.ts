
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

export type EvidenceLoadStatus = "idle" | "loading" | "success" | "error";
export type EvidencePolicyMode = "off" | "measure" | "advisory" | "block";

export interface EvidencePolicy {
  policy: EvidencePolicyMode;
  maxStopBlocks: number;
}

export interface EvidenceRecord {
  source: string;
  command?: string;
  status: string;
  summary?: string;
  at?: string;
}

export interface EvidenceRecentItem {
  kind: string;
  createdAt: string;
  workDir?: string;
  workspace?: string;
  provider?: string;
  model?: string;
  status: string;
  source: string;
  command?: string;
  gate?: string;
  mode?: string;
  summary?: string;
  editedFiles?: string[];
  evidence?: EvidenceRecord[];
  evidenceCount: number;
  editedFileCount?: number;
  taskCount?: number;
  completed?: number;
  failed?: number;
  receiptPath: string;
}

export interface EvidenceRecent {
  baseDir: string;
  scope: string;
  workDir?: string;
  items: EvidenceRecentItem[];
}

interface EvidenceState {
  status: EvidenceLoadStatus;
  policy: EvidencePolicy | null;
  recent: EvidenceRecent | null;
  error: string | null;
}

export interface EvidenceStatusState extends EvidenceState {
  refresh: () => Promise<void>;
  updatePolicy: (policy: EvidencePolicyMode) => Promise<void>;
}

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

export function useEvidenceStatus(apiUrl: string): EvidenceStatusState {
  const baseUrl = useMemo(() => normalizeApiUrl(apiUrl), [apiUrl]);
  const abortRef = useRef<AbortController | null>(null);
  const [state, setState] = useState<EvidenceState>({
    status: "idle",
    policy: null,
    recent: null,
    error: null,
  });

  const refresh = useCallback(async () => {
    if (!baseUrl) {
      setState((current) => ({ ...current, status: "error", error: "API base URL is empty" }));
      return;
    }

    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    const timeout = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS);

    setState((current) => ({
      ...current,
      status: current.policy || current.recent ? "success" : "loading",
      error: null,
    }));

    try {
      const [policy, recent] = await Promise.all([
        fetchJson<EvidencePolicy>(baseUrl, "/api/evidence/policy", controller.signal),
        fetchJson<EvidenceRecent>(baseUrl, "/api/evidence/recent?limit=8", controller.signal),
      ]);
      if (!isEvidencePolicy(policy) || !isEvidenceRecent(recent)) {
        throw new Error("Evidence API response is not available on this proxy");
      }
      setState({ status: "success", policy, recent, error: null });
    } catch (error) {
      if (controller.signal.aborted) return;
      setState((current) => ({
        ...current,
        status: "error",
        error: error instanceof Error ? error.message : "Evidence API unavailable",
      }));
    } finally {
      clearTimeout(timeout);
      if (abortRef.current === controller) {
        abortRef.current = null;
      }
    }
  }, [baseUrl]);

  const updatePolicy = useCallback(
    async (policy: EvidencePolicyMode) => {
      if (!baseUrl) return;
      const response = await fetch(`${baseUrl}/api/evidence/policy`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          policy,
          maxStopBlocks: state.policy?.maxStopBlocks ?? 2,
        }),
      });
      if (!response.ok) {
        throw new Error(`/api/evidence/policy returned ${response.status}`);
      }
      const nextPolicy = (await response.json()) as EvidencePolicy;
      setState((current) => ({ ...current, policy: nextPolicy, error: null }));
    },
    [baseUrl, state.policy?.maxStopBlocks]
  );

  useEffect(() => {
    refresh();
    return () => {
      abortRef.current?.abort();
    };
  }, [refresh]);

  return { ...state, refresh, updatePolicy };
}

function isEvidencePolicy(value: unknown): value is EvidencePolicy {
  return (
    typeof value === "object" &&
    value !== null &&
    typeof (value as EvidencePolicy).policy === "string" &&
    typeof (value as EvidencePolicy).maxStopBlocks === "number"
  );
}

function isEvidenceRecent(value: unknown): value is EvidenceRecent {
  return (
    typeof value === "object" &&
    value !== null &&
    Array.isArray((value as EvidenceRecent).items)
  );
}
