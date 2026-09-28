import { getLang } from '../lib/i18n';
import {
  MessageSquare,
  History,
  Settings,
  Sun,
  Moon,
} from 'lucide-react';

interface NavItem {
  id: string;
  label: string;
  labelKo: string;
  section: 'top' | 'bottom';
  icon: typeof MessageSquare;
}

// Three destinations (plan §7): files live in the workspace side panel and the
// team runner is a workspace mode, so both highlight "Workspace".
const items: NavItem[] = [
  { id: 'chat', label: 'Workspace', labelKo: '작업 공간', section: 'top', icon: MessageSquare },
  { id: 'history', label: 'History', labelKo: '기록', section: 'bottom', icon: History },
  { id: 'settings', label: 'Settings', labelKo: '설정', section: 'bottom', icon: Settings },
];

interface Props {
  active: string;
  onNavigate: (id: string) => void;
  onThemeToggle: () => void;
  theme: 'dark' | 'light';
  /** Destinations with a pending decision (e.g. a tool approval) get a badge. */
  attention?: Partial<Record<string, boolean>>;
}

function normalizeActiveTab(active: string): string {
  if (active === 'files' || active === 'team') return 'chat';
  if (active === 'costs' || active === 'kairos') return 'history';
  if (active === 'routes' || active === 'memory') return 'settings';
  return active;
}

export function ActivityBar({ active, onNavigate, onThemeToggle, theme, attention = {} }: Props) {
  const ko = getLang() === 'ko';
  const currentTab = normalizeActiveTab(active);

  const renderItem = (item: NavItem) => {
    const IconComponent = item.icon;
    const isSelected = currentTab === item.id;
    const needsAttention = Boolean(attention[item.id]);
    const label = ko ? item.labelKo : item.label;
    const accessibleLabel = needsAttention ? `${label} — ${ko ? '승인 대기' : 'approval pending'}` : label;
    return (
      <button
        key={item.id}
        type="button"
        onClick={() => onNavigate(item.id)}
        title={accessibleLabel}
        aria-label={accessibleLabel}
        aria-current={isSelected ? 'page' : undefined}
        className={`relative w-10 h-10 rounded-lg flex items-center justify-center transition-all ${
          isSelected
            ? 'bg-[var(--color-accent)]/15 text-[var(--color-accent)]'
            : 'text-[var(--color-text2)] hover:bg-[var(--color-surface2)] hover:text-[var(--color-text)]'
        }`}
      >
        <IconComponent aria-hidden="true" className="w-4 h-4" />
        {needsAttention && (
          <span
            aria-hidden="true"
            data-approval-badge={item.id}
            className="absolute top-1.5 right-1.5 w-2 h-2 rounded-full bg-[var(--color-warning)] ring-2 ring-[var(--color-surface)]"
          />
        )}
      </button>
    );
  };

  return (
    <nav aria-label={ko ? '주 메뉴' : 'Main'} className="w-12 bg-[var(--color-surface)] border-r border-[var(--color-border)] flex flex-col items-center py-2 shrink-0 h-[calc(100vh-24px)] sticky top-0">
      <div aria-hidden="true" className="w-7 h-7 rounded-md bg-[var(--color-accent)] flex items-center justify-center text-[var(--color-bg)] text-[10px] font-bold mb-4">
        C
      </div>

      <div className="flex-1 flex flex-col items-center gap-1">
        {items.filter((i) => i.section === 'top').map(renderItem)}
      </div>

      <div className="flex flex-col items-center gap-1 mb-1">
        {items.filter((i) => i.section === 'bottom').map(renderItem)}
        <button
          type="button"
          onClick={onThemeToggle}
          title={ko ? '테마 전환' : 'Toggle theme'}
          aria-label={ko ? '테마 전환' : 'Toggle theme'}
          className="w-10 h-10 rounded-lg flex items-center justify-center text-[var(--color-text2)] hover:bg-[var(--color-surface2)] transition-colors"
        >
          {theme === 'dark' ? <Sun aria-hidden="true" className="w-4 h-4" /> : <Moon aria-hidden="true" className="w-4 h-4" />}
        </button>
      </div>
    </nav>
  );
}
