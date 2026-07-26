import { t, getLang, setLang, type Lang, type TransKey } from '../lib/i18n';

interface NavItem {
  id: string;
  labelKey: TransKey;
  icon: string;
}

const items: NavItem[] = [
  { id: 'chat', labelKey: 'nav.chat', icon: '💬' },
  { id: 'workspace', labelKey: 'nav.workspace', icon: '📂' },
  { id: 'explorer', labelKey: 'nav.explorer', icon: '📁' },
  { id: 'settings', labelKey: 'nav.settings', icon: '⚙️' },
  { id: 'routes', labelKey: 'nav.routes', icon: '🔀' },
  { id: 'costs', labelKey: 'nav.costs', icon: '📊' },
  { id: 'kairos', labelKey: 'nav.kairos', icon: '🤖' },
  { id: 'memory', labelKey: 'nav.memory', icon: '🧠' },
  { id: 'team', labelKey: 'nav.team', icon: '👥' },
];

interface Props {
  active: string;
  onNavigate: (id: string) => void;
  onLangChange: () => void;
  onThemeToggle: () => void;
  theme: 'dark' | 'light';
  status: { provider: string; model: string; router: boolean } | null;
}

export function Sidebar({ active, onNavigate, onLangChange, onThemeToggle, theme, status }: Props) {
  const lang = getLang();

  function toggleLang() {
    const next: Lang = lang === 'ko' ? 'en' : 'ko';
    setLang(next);
    onLangChange();
  }

  return (
    <aside className="w-56 shrink-0 bg-[var(--color-surface)] border-r border-[var(--color-border)] flex flex-col h-screen sticky top-0">
      {/* Logo */}
      <div className="p-4 border-b border-[var(--color-border)]">
        <div className="flex items-center gap-2">
          <div className="w-8 h-8 rounded-lg bg-gradient-to-br from-[var(--color-accent)] to-purple-400 flex items-center justify-center text-white text-sm font-bold">
            A
          </div>
          <div>
            <div className="text-sm font-semibold text-[var(--color-text)]">AniClew</div>
            <div className="text-[10px] text-[var(--color-text2)]">v1.0.0</div>
          </div>
        </div>
      </div>

      {/* Nav */}
      <nav className="flex-1 p-2 space-y-1">
        {items.map((item) => (
          <button
            key={item.id}
            onClick={() => onNavigate(item.id)}
            className={`w-full flex items-center gap-3 px-3 py-2.5 rounded-lg text-sm transition-colors ${
              active === item.id
                ? 'bg-[var(--color-surface2)] text-[var(--color-text)]'
                : 'text-[var(--color-text2)] hover:bg-[var(--color-surface2)] hover:text-[var(--color-text)]'
            }`}
          >
            <span className="text-base">{item.icon}</span>
            {t(item.labelKey)}
          </button>
        ))}
      </nav>

      {/* Language + Theme Toggle */}
      <div className="px-3 py-2 border-t border-[var(--color-border)] space-y-1">
        <button
          onClick={toggleLang}
          className="w-full flex items-center justify-center gap-2 px-3 py-1.5 rounded-lg text-xs text-[var(--color-text2)] hover:bg-[var(--color-surface2)] hover:text-[var(--color-text)] transition-colors"
        >
          <span>🌐</span>
          {lang === 'ko' ? '한국어 → English' : 'English → 한국어'}
        </button>
        <button
          onClick={onThemeToggle}
          className="w-full flex items-center justify-center gap-2 px-3 py-1.5 rounded-lg text-xs text-[var(--color-text2)] hover:bg-[var(--color-surface2)] hover:text-[var(--color-text)] transition-colors"
        >
          <span>{theme === 'dark' ? '☀️' : '🌙'}</span>
          {theme === 'dark' ? 'Light Mode' : 'Dark Mode'}
        </button>
      </div>

      {/* Status */}
      <div className="p-3 border-t border-[var(--color-border)] text-xs text-[var(--color-text2)]">
        {status ? (
          <>
            <div className="flex items-center gap-1.5 mb-1">
              <div className="w-2 h-2 rounded-full bg-[var(--color-green)] animate-pulse" />
              {t('status.online')}
            </div>
            <div className="truncate">{status.provider} / {status.model}</div>
            <div>{status.router ? t('status.routerOn') : t('status.singleModel')}</div>
          </>
        ) : (
          <div className="flex items-center gap-1.5">
            <div className="w-2 h-2 rounded-full bg-[var(--color-red)]" />
            {t('status.offline')}
          </div>
        )}
      </div>
    </aside>
  );
}
