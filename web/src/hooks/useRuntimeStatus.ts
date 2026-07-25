import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

export type RuntimeProviderGroup = 'default' | 'claude' | 'codex' | 'local';
export type RuntimeAccountStatus = 'healthy' | 'unhealthy';
export type RuntimeLoadStatus = 'idle' | 'loading' | 'success' | 'error';

export interface RuntimeQuotaWindow {
  used: number;
  limit: number;
  resetAt?: string;
}

export interface RuntimeAccountState {
  id: string;
  displayName?: string;
  provider: string;
  group: RuntimeProviderGroup;
  status: RuntimeAccountStatus;
  fiveHour: RuntimeQuotaWindow;
  sevenDay: RuntimeQuotaWindow;
  rateLimit?: RuntimeQuotaWindow;
  cooldownUntil?: string;
  observedAt?: string;
  allowStale?: boolean;
}

export interface RuntimeAgentLease {
  id: string;
  sessionId: string;
  agentId?: string;
  accountId: string;
  provider: string;
  model: string;
  group: RuntimeProviderGroup;
  createdAt: string;
  expiresAt?: string;
}

export interface RuntimeActiveTarget {
  provider: string;
  providerDisplayName?: string;
  model: string;
  modelGroup: RuntimeProviderGroup;
  providerGroup: RuntimeProviderGroup;
  selectionGroup: RuntimeProviderGroup;
}

export interface RuntimeSchedulerPolicy {
  fiveHourMax: number;
  sevenDayMax: number;
  urgencyMax: number;
  switchMargin: number;
  staleAfterSeconds: number;
}

export interface RuntimeSelectionDecision {
  action: 'stay' | 'switch' | 'none' | string;
  accountId?: string;
  score?: number;
  reason: string;
}

export interface RuntimeProviderModel {
  id: string;
  displayName: string;
  group: RuntimeProviderGroup;
  contextWindow?: number;
  maxOutput?: number;
}

export interface RuntimeProviderInfo {
  name: string;
  displayName: string;
  group: RuntimeProviderGroup;
  models: RuntimeProviderModel[];
}

export interface RuntimeQuotaCollectorInfo {
  index: number;
  name?: string;
  type: 'file' | 'http' | string;
  path?: string;
  url?: string;
  headerNames?: string[];
  intervalSeconds?: number;
  timeoutSeconds?: number;
  enabled: boolean;
}

export interface RuntimeStatus {
  generatedAt: string;
  active: RuntimeActiveTarget;
  routerEnabled: boolean;
  scheduler: RuntimeSchedulerPolicy;
  accounts: RuntimeAccountState[];
  selection: RuntimeSelectionDecision;
  providers: RuntimeProviderInfo[];
  leases?: RuntimeAgentLease[];
  quotaCollectors?: RuntimeQuotaCollectorInfo[];
  quotaSource: string;
}

export interface RuntimeStatusState {
  status: RuntimeLoadStatus;
  data: RuntimeStatus | null;
  error: string | null;
  refresh: () => Promise<void>;
}

const DEFAULT_REFRESH_MS = 15_000;
const REQUEST_TIMEOUT_MS = 5_000;

export function formatRuntimeGroup(group?: RuntimeProviderGroup) {
  switch (group) {
    case 'claude':
      return 'Claude';
    case 'codex':
      return 'Codex';
    case 'local':
      return 'Local';
    case 'default':
    default:
      return 'Default';
  }
}

function normalizeApiUrl(apiUrl: string) {
  return apiUrl.trim().replace(/\/+$/, '');
}

export function useRuntimeStatus(apiUrl: string, refreshMs = DEFAULT_REFRESH_MS): RuntimeStatusState {
  const baseUrl = useMemo(() => normalizeApiUrl(apiUrl), [apiUrl]);
  const abortRef = useRef<AbortController | null>(null);
  const [state, setState] = useState<Omit<RuntimeStatusState, 'refresh'>>({
    status: 'idle',
    data: null,
    error: null,
  });

  const refresh = useCallback(async () => {
    if (!baseUrl) {
      setState((current) => ({
        ...current,
        status: 'error',
        error: 'API base URL is empty',
      }));
      return;
    }

    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    const timeout = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS);

    setState((current) => ({
      ...current,
      status: current.data ? 'success' : 'loading',
      error: null,
    }));

    try {
      const response = await fetch(`${baseUrl}/api/runtime`, {
        cache: 'no-store',
        signal: controller.signal,
      });
      if (!response.ok) {
        throw new Error(`Runtime API returned ${response.status}`);
      }
      const data = (await response.json()) as RuntimeStatus;
      setState({ status: 'success', data, error: null });
    } catch (error) {
      if (controller.signal.aborted) return;
      setState((current) => ({
        status: 'error',
        data: current.data,
        error: error instanceof Error ? error.message : 'Runtime API unavailable',
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
