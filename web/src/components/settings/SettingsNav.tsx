import { cn } from '@/lib/utils';

/**
 * Every section the settings surface knows about. Sections not yet ported from
 * the previous UI stay in the union so cross-section navigation props type-check;
 * `PORTED_SECTIONS` is the list actually rendered today.
 */
export type SettingsSection =
  | 'overview'
  | 'accounts'
  | 'providers'
  | 'routing'
  | 'scheduler'
  | 'agents'
  | 'commands'
  | 'skills'
  | 'integrations'
  | 'loops'
  | 'verification'
  | 'handoffs'
  | 'permissions'
  | 'memory'
  | 'privacy'
  | 'appearance'
  | 'shortcuts'
  | 'advanced';

export interface NavItem {
  id: SettingsSection;
  label: string;
  shortLabel: string;
}

const PORTED_SECTIONS: NavItem[] = [
  { id: 'overview', label: 'Overview', shortLabel: 'Overview' },
  { id: 'scheduler', label: 'Scheduler', shortLabel: 'Scheduler' },
  { id: 'accounts', label: 'Accounts', shortLabel: 'Accounts' },
  { id: 'providers', label: 'Providers', shortLabel: 'Providers' },
  { id: 'routing', label: 'Routing', shortLabel: 'Routing' },
];

interface SettingsNavProps {
  active: SettingsSection;
  onSelect: (section: SettingsSection) => void;
  items?: NavItem[];
  className?: string;
}

export function SettingsNav({ active, onSelect, items = PORTED_SECTIONS, className }: SettingsNavProps) {
  return (
    <nav className={cn('flex flex-wrap gap-1', className)} aria-label="Settings sections">
      {items.map((item) => {
        const selected = item.id === active;
        return (
          <button
            key={item.id}
            type="button"
            onClick={() => onSelect(item.id)}
            aria-current={selected ? 'page' : undefined}
            className={cn(
              'rounded-md px-3 py-1.5 text-xs transition-colors',
              selected
                ? 'bg-brand-500/15 text-brand-400'
                : 'text-surface-400 hover:bg-surface-900 hover:text-surface-200',
            )}
          >
            {item.label}
          </button>
        );
      })}
    </nav>
  );
}
