import type { KeyboardEvent } from 'react';

export type WorkspaceMode = 'single' | 'team';

const MODES: { id: WorkspaceMode; label: string; hint: string }[] = [
  { id: 'single', label: '대화', hint: '한 에이전트와 대화하며 작업합니다' },
  { id: 'team', label: '팀', hint: '작업 계획을 wave로 나눠 여러 worker가 실행합니다' },
];

/** Workspace execution mode (plan §5.2.B): the Team page is a mode of the workspace, not a separate destination. */
export function WorkspaceModeSwitch({
  mode,
  onChange,
  attention = {},
}: {
  mode: WorkspaceMode;
  onChange: (mode: WorkspaceMode) => void;
  /** Modes with a pending approval get a badge so the decision is findable. */
  attention?: Partial<Record<WorkspaceMode, boolean>>;
}) {
  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return;
    event.preventDefault();
    const index = MODES.findIndex((item) => item.id === mode);
    const next = MODES[(index + (event.key === 'ArrowRight' ? 1 : MODES.length - 1)) % MODES.length];
    onChange(next.id);
  };

  return (
    <div
      role="radiogroup"
      aria-label="실행 모드"
      onKeyDown={onKeyDown}
      className="inline-flex items-center rounded-lg border border-[var(--color-border)] bg-[var(--color-bg)] p-0.5 text-[11px]"
    >
      {MODES.map((item) => {
        const selected = item.id === mode;
        const pending = Boolean(attention[item.id]);
        return (
          <button
            key={item.id}
            type="button"
            role="radio"
            aria-checked={selected}
            tabIndex={selected ? 0 : -1}
            title={pending ? `${item.hint} (승인 대기)` : item.hint}
            aria-label={pending ? `${item.label} — 승인 대기` : item.label}
            onClick={() => {
              if (!selected) onChange(item.id);
            }}
            className={`relative px-2.5 py-1 rounded-md font-medium transition-colors ${
              selected
                ? 'bg-[var(--color-surface2)] text-[var(--color-text)]'
                : 'text-[var(--color-text2)] hover:text-[var(--color-text)]'
            }`}
          >
            {item.label}
            {pending && (
              <span
                aria-hidden="true"
                data-mode-approval-badge={item.id}
                className="absolute -top-0.5 -right-0.5 w-1.5 h-1.5 rounded-full bg-[var(--color-warning)]"
              />
            )}
          </button>
        );
      })}
    </div>
  );
}
