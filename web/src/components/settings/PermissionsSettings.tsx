import { useState } from 'react';
import { Shield, ShieldAlert, Lock } from 'lucide-react';
import type { SessionExecutionMode } from '@/lib/sessions';
import { loadPreferredExecutionMode, savePreferredExecutionMode } from '@/lib/executionMode';

interface ModeOption {
  mode: SessionExecutionMode;
  name: string;
  nameKo: string;
  descKo: string;
  desc: string;
}

// Descriptions follow internal/agent/permission.go (EvaluatePermission) and
// tool_dispatch.go (full-mode grants); keep them in sync with that behaviour.
const MODES: ModeOption[] = [
  {
    mode: 'read-only',
    name: 'Read only',
    nameKo: '읽기 전용',
    descKo: '파일을 수정하지 않습니다. 읽기·검색·웹 조회 도구만 쓰고, 명령은 파일시스템 격리가 있을 때 읽기 전용으로 분류된 것만 실행합니다.',
    desc: 'No file changes. Read, search and web lookup tools only; commands run only when classified read-only and filesystem isolation is available.',
  },
  {
    mode: 'workspace',
    name: 'Workspace',
    nameKo: '작업 폴더 (서버 기본값)',
    descKo: '작업 폴더 안에서만 동작합니다. 읽기·검색과 보통 위험도 도구는 자동으로 실행하고, 위험한 도구는 승인을 받습니다. 파일시스템 격리가 없으면 Bash와 Git도 매번 승인을 받습니다.',
    desc: 'Stays inside the workspace. Reads, searches and moderate tools run automatically; risky tools ask for approval. Without filesystem isolation, Bash and Git always ask.',
  },
  {
    mode: 'full',
    name: 'Full access',
    nameKo: '전체 권한',
    descKo: '현재 OS 사용자 권한으로 호스트에서 실행하며 작업 폴더 밖 경로도 허용합니다. 모든 내장 도구가 확인 없이 실행되고, MCP·플러그인 도구와 호스트 상호작용 도구도 묻지 않고 해당 호출에만 승인이 자동 발급됩니다. 금지 규칙과 명시적 거부 규칙만 계속 적용됩니다.',
    desc: 'Runs on the host as the current OS user, including paths outside the workspace. All built-in tools run without asking, and MCP/plugin and host-interaction tools get an automatic per-call grant. Only the hard safety rules and explicit deny rules still apply.',
  },
];

export function PermissionsSettings() {
  const [selected, setSelected] = useState<SessionExecutionMode | null>(loadPreferredExecutionMode);
  const [fullAcknowledged, setFullAcknowledged] = useState(selected === 'full');

  const choose = (mode: SessionExecutionMode | null) => {
    if (mode === 'full' && !fullAcknowledged) return;
    setSelected(mode);
    savePreferredExecutionMode(mode);
  };

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-base font-semibold text-[var(--color-text)]">실행 권한</h2>
        <p className="text-xs text-[var(--color-text2)] mt-1 leading-relaxed">
          다음 턴부터 에이전트 요청에 이 모드를 보냅니다. 서버가 모드를 검증하고, 실제 적용된 모드는 채팅 헤더에 표시됩니다.
          명시적으로 고르지 않으면 세션에 저장된 모드 또는 서버 기본값(작업 폴더)을 따릅니다.
        </p>
      </div>

      <fieldset className="grid gap-3.5">
        <legend className="sr-only">Execution mode</legend>
        {MODES.map((m) => {
          const isSelected = selected === m.mode;
          const disabled = m.mode === 'full' && !fullAcknowledged;
          return (
            <label
              key={m.mode}
              className={`p-4 rounded-xl border transition-all block ${disabled ? 'cursor-not-allowed' : 'cursor-pointer'} ${
                isSelected
                  ? 'border-[var(--color-accent)] bg-[var(--color-surface2)]/40 shadow-sm'
                  : 'border-[var(--color-border)] bg-[var(--color-surface)]'
              }`}
            >
              <div className="flex items-start gap-3">
                <input
                  type="radio"
                  name="execution-mode"
                  value={m.mode}
                  checked={isSelected}
                  disabled={disabled}
                  onChange={() => choose(m.mode)}
                  className="mt-1 accent-[var(--color-accent)]"
                />
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    {m.mode === 'full' ? (
                      <ShieldAlert aria-hidden="true" className="w-4 h-4 text-[var(--color-warning)]" />
                    ) : m.mode === 'read-only' ? (
                      <Lock aria-hidden="true" className="w-4 h-4 text-[var(--color-text2)]" />
                    ) : (
                      <Shield aria-hidden="true" className="w-4 h-4 text-[var(--color-accent)]" />
                    )}
                    <span className="font-semibold text-xs text-[var(--color-text)]">{m.nameKo}</span>
                    <span className="text-[11px] text-[var(--color-text2)]">{m.name} · <span className="font-mono">{m.mode}</span></span>
                  </div>
                  <p className="text-xs text-[var(--color-text)] mt-2 leading-relaxed">{m.descKo}</p>
                  <p className="text-[11px] text-[var(--color-text2)] mt-1">{m.desc}</p>
                </div>
              </div>
            </label>
          );
        })}
      </fieldset>

      <div className="p-4 rounded-xl border border-[var(--color-warning)]/40 bg-[var(--color-surface)] space-y-2">
        <label className="flex items-start gap-2 text-xs text-[var(--color-text)] cursor-pointer">
          <input
            type="checkbox"
            checked={fullAcknowledged}
            onChange={(e) => {
              setFullAcknowledged(e.target.checked);
              if (!e.target.checked && selected === 'full') choose(null);
            }}
            className="mt-0.5 accent-[var(--color-warning)]"
          />
          <span>
            전체 권한 모드에서는 내장 도구와 MCP·플러그인 도구가 확인 없이 실행된다는 점을 이해했습니다.
            <span className="block text-[11px] text-[var(--color-text2)] mt-0.5">이 확인을 해야 전체 권한을 고를 수 있습니다.</span>
          </span>
        </label>
      </div>

      <div className="flex items-center justify-between gap-3 text-xs text-[var(--color-text2)]">
        <span>
          현재 선택: <span className="font-mono text-[var(--color-text)]">{selected ?? '선택 없음 (세션 또는 서버 기본값)'}</span>
        </span>
        {selected && (
          <button
            type="button"
            onClick={() => choose(null)}
            className="px-2.5 py-1 rounded border border-[var(--color-border)] hover:bg-[var(--color-surface2)] text-[var(--color-text)]"
          >
            선택 해제
          </button>
        )}
      </div>
    </div>
  );
}
