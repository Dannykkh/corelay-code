import type React from 'react';
import { cn } from '@/lib/utils';

export type ActionTone = 'neutral' | 'good' | 'warn' | 'danger';

export interface SectionActionMetric {
  label: string;
  value: string;
  tone?: ActionTone;
}

export interface SectionAction {
  label: string;
  icon?: React.ElementType;
  onClick: () => void | Promise<void>;
  disabled?: boolean;
  tone?: ActionTone;
}

function toneClasses(tone: ActionTone = 'neutral') {
  switch (tone) {
    case 'good':
      return 'border-green-500/20 bg-green-500/10 text-green-400';
    case 'warn':
      return 'border-amber-500/20 bg-amber-500/10 text-amber-400';
    case 'danger':
      return 'border-red-500/20 bg-red-500/10 text-red-400';
    default:
      return 'border-surface-800 bg-surface-900 text-surface-400';
  }
}

interface SectionActionStripProps {
  icon: React.ElementType;
  eyebrow?: string;
  title: string;
  description: string;
  stateLabel?: string;
  stateTone?: ActionTone;
  metrics?: SectionActionMetric[];
  actions?: SectionAction[];
}

/** Banner that states the single next action for a settings section. */
export function SectionActionStrip({
  icon: Icon,
  eyebrow = 'Next action',
  title,
  description,
  stateLabel,
  stateTone = 'neutral',
  metrics = [],
  actions,
}: SectionActionStripProps) {
  return (
    <div className="mb-5 rounded-lg border border-surface-800 bg-surface-900/60 p-4">
      <div className="flex items-start gap-3">
        <span className={cn('mt-0.5 rounded-md border p-2', toneClasses(stateTone))}>
          <Icon className="h-4 w-4" />
        </span>

        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-[10px] font-medium uppercase tracking-wide text-surface-500">
              {eyebrow}
            </span>
            {stateLabel && (
              <span className={cn('rounded-full border px-2 py-0.5 text-[10px]', toneClasses(stateTone))}>
                {stateLabel}
              </span>
            )}
          </div>

          <h3 className="mt-1 truncate text-sm font-semibold text-surface-100">{title}</h3>
          <p className="mt-1 text-xs leading-relaxed text-surface-400">{description}</p>

          {metrics.length > 0 && (
            <dl className="mt-3 flex flex-wrap gap-x-6 gap-y-2">
              {metrics.map((metric) => (
                <div key={metric.label} className="min-w-0">
                  <dt className="text-[10px] uppercase tracking-wide text-surface-500">{metric.label}</dt>
                  <dd
                    className={cn(
                      'truncate text-xs font-medium',
                      metric.tone === 'good' && 'text-green-400',
                      metric.tone === 'warn' && 'text-amber-400',
                      metric.tone === 'danger' && 'text-red-400',
                      (!metric.tone || metric.tone === 'neutral') && 'text-surface-200',
                    )}
                  >
                    {metric.value}
                  </dd>
                </div>
              ))}
            </dl>
          )}
        </div>

        {actions && actions.length > 0 && (
          <div className="flex shrink-0 flex-col items-stretch gap-2">
            {actions.map((action) => {
              const ActionIcon = action.icon;
              return (
                <button
                  key={action.label}
                  type="button"
                  onClick={() => void action.onClick()}
                  disabled={action.disabled}
                  className={cn(
                    'inline-flex items-center justify-center gap-1.5 rounded-md border px-3 py-1.5 text-xs transition-colors',
                    toneClasses(action.tone),
                    action.disabled
                      ? 'cursor-not-allowed opacity-50'
                      : 'hover:border-surface-700 hover:text-surface-100',
                  )}
                >
                  {ActionIcon && <ActionIcon className="h-3.5 w-3.5" />}
                  {action.label}
                </button>
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
}

interface SettingRowProps {
  label: string;
  description?: string;
  children: React.ReactNode;
  className?: string;
  stack?: boolean;
  scope?: string;
  risk?: 'low' | 'medium' | 'high';
}

const riskClasses: Record<NonNullable<SettingRowProps['risk']>, string> = {
  low: 'border-surface-800 bg-surface-900 text-surface-400',
  medium: 'border-amber-500/20 bg-amber-500/10 text-amber-400',
  high: 'border-red-500/20 bg-red-500/10 text-red-400',
};

/** One labelled control. `stack` puts the control below the label instead of beside it. */
export function SettingRow({
  label,
  description,
  children,
  className,
  stack = false,
  scope,
  risk,
}: SettingRowProps) {
  return (
    <div
      className={cn(
        'border-b border-surface-800/60 py-3 last:border-b-0',
        stack ? 'space-y-2' : 'flex items-start justify-between gap-6',
        className,
      )}
    >
      <div className="min-w-0">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-sm text-surface-100">{label}</span>
          {scope && (
            <span className="rounded border border-surface-800 bg-surface-900 px-1.5 py-0.5 text-[10px] text-surface-500">
              {scope}
            </span>
          )}
          {risk && (
            <span className={cn('rounded border px-1.5 py-0.5 text-[10px] capitalize', riskClasses[risk])}>
              {risk} risk
            </span>
          )}
        </div>
        {description && <p className="mt-1 text-xs leading-relaxed text-surface-400">{description}</p>}
      </div>

      <div className={cn(stack ? 'w-full' : 'shrink-0')}>{children}</div>
    </div>
  );
}

interface SectionHeaderProps {
  title: string;
  description?: string;
  eyebrow?: string;
  onReset?: () => void;
  children?: React.ReactNode;
}

export function SectionHeader({ title, description, eyebrow, onReset, children }: SectionHeaderProps) {
  return (
    <div className="mb-4 flex items-start justify-between gap-4 border-b border-surface-800/80 pb-4">
      <div className="min-w-0">
        {eyebrow && (
          <div className="text-[10px] font-medium uppercase tracking-wide text-surface-500">{eyebrow}</div>
        )}
        <h2 className="text-base font-semibold text-surface-100">{title}</h2>
        {description && <p className="mt-1 text-xs leading-relaxed text-surface-400">{description}</p>}
      </div>

      <div className="flex shrink-0 items-center gap-2">
        {children}
        {onReset && (
          <button
            type="button"
            onClick={onReset}
            className="rounded border border-surface-800 px-2 py-1 text-xs text-surface-400 transition-colors hover:border-surface-700 hover:text-surface-200"
          >
            Reset
          </button>
        )}
      </div>
    </div>
  );
}

interface ToggleProps {
  checked: boolean;
  onChange: (checked: boolean) => void;
  disabled?: boolean;
}

export function Toggle({ checked, onChange, disabled = false }: ToggleProps) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={cn(
        'relative inline-flex h-5 w-9 items-center rounded-full transition-colors',
        checked ? 'bg-brand-500' : 'bg-surface-700',
        disabled && 'cursor-not-allowed opacity-50',
      )}
    >
      <span
        className={cn(
          'inline-block h-3.5 w-3.5 rounded-full bg-white transition-transform',
          checked ? 'translate-x-4.5' : 'translate-x-1',
        )}
      />
    </button>
  );
}

interface SliderProps {
  value: number;
  min: number;
  max: number;
  step?: number;
  onChange: (value: number) => void;
  showValue?: boolean;
  unit?: string;
  className?: string;
}

export function Slider({
  value,
  min,
  max,
  step = 1,
  onChange,
  showValue = true,
  unit = '',
  className,
}: SliderProps) {
  return (
    <div className={cn('flex items-center gap-3', className)}>
      <input
        type="range"
        min={min}
        max={max}
        step={step}
        value={value}
        onChange={(event) => onChange(Number(event.target.value))}
        className="h-1 w-40 cursor-pointer appearance-none rounded-full bg-surface-700 accent-brand-500"
      />
      {showValue && (
        <span className="w-14 shrink-0 text-right text-xs tabular-nums text-surface-300">
          {value}
          {unit}
        </span>
      )}
    </div>
  );
}
