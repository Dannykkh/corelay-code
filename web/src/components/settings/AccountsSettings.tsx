
import { useState } from "react";
import { CheckCircle, Eye, EyeOff, KeyRound, Loader2, RefreshCw, XCircle } from "lucide-react";
import { useSettingsStore } from "@/lib/settings-store";
import { formatRuntimeGroup, useRuntimeStatus } from "@/hooks/useRuntimeStatus";
import { SectionActionStrip, SectionHeader, SettingRow } from "./SettingRow";
import { cn } from "@/lib/utils";

type ConnectionStatus = "idle" | "checking" | "ok" | "error";

function formatQuota(window?: { used: number; limit: number; resetAt?: string }) {
  if (!window || window.limit <= 0) {
    return "Awaiting provider headers";
  }
  const usedPercent = Math.round((window.used / window.limit) * 100);
  const reset = window.resetAt ? `, resets ${new Date(window.resetAt).toLocaleTimeString()}` : "";
  return `${usedPercent}% used${reset}`;
}

function formatLeaseExpiry(expiresAt?: string) {
  if (!expiresAt) {
    return "No expiry";
  }
  const expires = new Date(expiresAt).getTime();
  const remainingMs = expires - Date.now();
  if (Number.isNaN(expires) || remainingMs <= 0) {
    return "Expired";
  }
  const minutes = Math.max(1, Math.round(remainingMs / 60000));
  if (minutes < 60) {
    return `${minutes}m left`;
  }
  return `${Math.round(minutes / 60)}h left`;
}

export function AccountsSettings() {
  const { settings, updateSettings } = useSettingsStore();
  const runtime = useRuntimeStatus(settings.apiUrl);
  const runtimeAccounts = runtime.data?.accounts ?? [];
  const runtimeLeases = runtime.data?.leases ?? [];
  const [showKey, setShowKey] = useState(false);
  const [connectionStatus, setConnectionStatus] = useState<ConnectionStatus>("idle");
  const [latencyMs, setLatencyMs] = useState<number | null>(null);

  async function checkConnection() {
    setConnectionStatus("checking");
    setLatencyMs(null);
    const start = Date.now();
    try {
      const res = await fetch(`${settings.apiUrl}/health`, {
        signal: AbortSignal.timeout(5000),
      });
      const ms = Date.now() - start;
      setLatencyMs(ms);
      setConnectionStatus(res.ok ? "ok" : "error");
    } catch {
      setConnectionStatus("error");
    }
  }

  const statusIcon = {
    idle: null,
    checking: <Loader2 className="h-4 w-4 animate-spin text-surface-400" />,
    ok: <CheckCircle className="h-4 w-4 text-green-400" />,
    error: <XCircle className="h-4 w-4 text-red-400" />,
  }[connectionStatus];

  const statusText = {
    idle: "Not checked",
    checking: "Checking...",
    ok: latencyMs !== null ? `Connected in ${latencyMs}ms` : "Connected",
    error: "Connection failed",
  }[connectionStatus];
  const runtimeOnline = runtime.status === "success" && Boolean(runtime.data);
  const hasCredential = Boolean(settings.apiKey);
  const accountState = runtimeOnline && runtimeAccounts.length > 0 ? "ready" : hasCredential ? "verify" : "needs setup";
  const accountTone = runtimeOnline && runtimeAccounts.length > 0 ? "good" : hasCredential ? "neutral" : "warn";
  const accountTitle =
    runtimeAccounts.length > 0
      ? "Runtime accounts are visible"
      : hasCredential
        ? "Credential is saved; verify the endpoint"
        : "Add a credential or start a runtime account";
  const accountDetail =
    runtimeAccounts.length > 0
      ? "Review account health, quota windows, and active session leases before long runs."
      : hasCredential
        ? "Run a connection check, then refresh runtime accounts if the proxy is already running."
        : runtime.error || "Paste a provider key below or start the local proxy with configured accounts.";

  return (
    <div>
      <SectionHeader
        eyebrow="Runtime"
        title="Accounts"
        description="Manage the identities and credentials that spend provider quota. Provider endpoints live under Providers."
      >
        <button
          onClick={runtime.refresh}
          className="flex h-11 w-11 items-center justify-center rounded border border-surface-800 text-surface-400 transition-colors hover:border-surface-700 hover:text-surface-200 active:translate-y-px"
          title="Refresh runtime accounts"
          type="button"
        >
          <RefreshCw className={cn("h-3.5 w-3.5", runtime.status === "loading" && "animate-spin")} />
        </button>
      </SectionHeader>

      <SectionActionStrip
        icon={KeyRound}
        title={accountTitle}
        description={accountDetail}
        stateLabel={accountState}
        stateTone={accountTone}
        metrics={[
          {
            label: "Credential",
            value: hasCredential ? `...${settings.apiKey.slice(-4)}` : "not saved",
            tone: hasCredential ? "good" : "warn",
          },
          {
            label: "Accounts",
            value: `${runtimeAccounts.length}`,
            tone: runtimeAccounts.length > 0 ? "good" : "neutral",
          },
          {
            label: "Leases",
            value: `${runtimeLeases.length}`,
            tone: runtimeLeases.length > 0 ? "good" : "neutral",
          },
        ]}
        actions={[
          {
            label: connectionStatus === "checking" ? "Checking endpoint" : "Check endpoint",
            icon: connectionStatus === "checking" ? Loader2 : CheckCircle,
            onClick: checkConnection,
            disabled: connectionStatus === "checking",
            tone: connectionStatus === "ok" ? "good" : connectionStatus === "error" ? "warn" : "neutral",
          },
          {
            label: runtime.status === "loading" ? "Refreshing accounts" : "Refresh accounts",
            icon: RefreshCw,
            onClick: runtime.refresh,
            disabled: runtime.status === "loading",
            tone: runtimeOnline ? "good" : "neutral",
          },
        ]}
      />

      <div className="mb-4 grid gap-2 md:grid-cols-2">
        <div className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
          <p className="text-sm font-medium text-surface-100">Claude API account</p>
          <p className="mt-1 text-xs text-surface-500">
            {settings.apiKey ? `Credential configured, ending ...${settings.apiKey.slice(-4)}` : "No key saved yet"}
          </p>
        </div>
        {runtimeAccounts.length > 0 ? (
          runtimeAccounts.map((account) => (
            <div key={account.id} className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
              <div className="flex items-start justify-between gap-3">
                <div className="min-w-0">
                  <p className="truncate text-sm font-medium text-surface-100">
                    {account.displayName || account.provider}
                  </p>
                  <p className="mt-1 font-mono text-xs text-surface-500">{account.id}</p>
                </div>
                <span
                  className={cn(
                    "rounded border px-1.5 py-0.5 text-[10px] uppercase tracking-[0.08em]",
                    account.status === "healthy"
                      ? "border-green-500/20 bg-green-500/10 text-green-300"
                      : "border-red-500/20 bg-red-500/10 text-red-300"
                  )}
                >
                  {account.status}
                </span>
              </div>
              <p className="mt-2 text-xs text-surface-500">
                {formatRuntimeGroup(account.group)} group. Rate limit: {formatQuota(account.rateLimit)}
              </p>
              {account.cooldownUntil && (
                <p className="mt-1 text-xs text-amber-300">
                  Cooling until {new Date(account.cooldownUntil).toLocaleTimeString()}
                </p>
              )}
            </div>
          ))
        ) : (
          <div className="rounded-lg border border-dashed border-surface-800 bg-surface-950/20 p-3">
            <p className="text-sm font-medium text-surface-300">Runtime accounts</p>
            <p className="mt-1 text-xs text-surface-500">
              {runtime.status === "loading"
                ? "Loading account inventory..."
                : runtime.error || "No runtime accounts reported yet."}
            </p>
          </div>
        )}
      </div>

      <div className="mb-4 border-t border-surface-800/80 pt-3">
        <div className="mb-2 flex items-center justify-between gap-3">
          <div>
            <p className="text-sm font-medium text-surface-100">Active session leases</p>
            <p className="mt-0.5 text-xs text-surface-500">
              {runtimeLeases.length > 0
                ? `${runtimeLeases.length} session${runtimeLeases.length === 1 ? "" : "s"} pinned`
                : runtime.status === "loading"
                  ? "Loading lease state"
                  : "No pinned sessions"}
            </p>
          </div>
          <span className="rounded border border-surface-800 bg-surface-950 px-2 py-1 font-mono text-xs text-surface-300">
            {runtimeLeases.length}
          </span>
        </div>
        {runtimeLeases.length > 0 && (
          <div className="grid gap-2 md:grid-cols-2">
            {runtimeLeases.map((lease) => (
              <div key={lease.id} className="rounded-lg border border-surface-800 bg-surface-950/30 p-3">
                <div className="flex items-start justify-between gap-3">
                  <div className="min-w-0">
                    <p className="truncate font-mono text-xs text-surface-200">{lease.sessionId}</p>
                    <p className="mt-1 truncate text-xs text-surface-500">{lease.accountId}</p>
                  </div>
                  <span className="rounded border border-brand-500/20 bg-brand-500/10 px-1.5 py-0.5 text-[10px] uppercase tracking-[0.08em] text-brand-200">
                    {formatRuntimeGroup(lease.group)}
                  </span>
                </div>
                <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-surface-500">
                  <span>{lease.provider}</span>
                  <span className="font-mono text-surface-400">{lease.model}</span>
                  <span>{formatLeaseExpiry(lease.expiresAt)}</span>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>

      <SettingRow
        label="API key"
        description="Stored locally by this web client. Keep it masked unless you are actively editing it."
        stack
        scope="Security"
        risk="high"
      >
        <div className="flex gap-2">
          <div className="relative flex-1">
            <input
              type={showKey ? "text" : "password"}
              value={settings.apiKey}
              onChange={(e) => updateSettings({ apiKey: e.target.value })}
              placeholder="sk-ant-..."
              className={cn(
                "min-h-11 w-full rounded-md border border-surface-700 bg-surface-800 px-3 py-2 pr-12 font-mono text-sm",
                "text-surface-200 placeholder-surface-600 focus:outline-none focus:ring-1 focus:ring-brand-500"
              )}
            />
            <button
              onClick={() => setShowKey((value) => !value)}
              className="absolute right-1 top-1/2 flex h-10 w-10 -translate-y-1/2 items-center justify-center rounded text-surface-500 transition-colors hover:bg-surface-700/60 hover:text-surface-300"
              title={showKey ? "Hide key" : "Show key"}
              type="button"
            >
              {showKey ? <EyeOff className="h-4 w-4" /> : <Eye className="h-4 w-4" />}
            </button>
          </div>
        </div>
      </SettingRow>

      <SettingRow
        label="Connection check"
        description="Verify that the currently configured provider endpoint is reachable before relying on it."
        scope="Provider"
        risk="low"
      >
        <div className="flex items-center gap-3">
          <div className="flex items-center gap-1.5">
            {statusIcon}
            <span
              className={cn(
                "text-xs",
                connectionStatus === "ok" && "text-green-400",
                connectionStatus === "error" && "text-red-400",
                connectionStatus === "idle" && "text-surface-500",
                connectionStatus === "checking" && "text-surface-400"
              )}
            >
              {statusText}
            </span>
          </div>
          <button
            onClick={checkConnection}
            disabled={connectionStatus === "checking"}
            className={cn(
              "min-h-11 rounded-md border border-surface-700 px-3 py-2 text-xs text-surface-300 transition-colors",
              "hover:bg-surface-800 hover:text-surface-100 active:translate-y-px",
              "disabled:cursor-not-allowed disabled:opacity-50"
            )}
            type="button"
          >
            Check
          </button>
        </div>
      </SettingRow>
    </div>
  );
}
