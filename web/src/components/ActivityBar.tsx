import { getLang } from '../lib/i18n';

interface NavItem {
  id: string;
  label: string;
  labelKo: string;
  section: 'top' | 'bottom';
}

const items: NavItem[] = [
  { id: 'chat', label: 'Chat', labelKo: '채팅', section: 'top' },
  { id: 'files', label: 'Files', labelKo: '파일', section: 'top' },
  { id: 'team', label: 'Team', labelKo: '팀', section: 'top' },
  { id: 'memory', label: 'Memory', labelKo: '메모리', section: 'top' },
  { id: 'costs', label: 'Activity', labelKo: '활동', section: 'bottom' },
  { id: 'routes', label: 'Routes', labelKo: '라우팅', section: 'bottom' },
  { id: 'kairos', label: 'Kairos', labelKo: '카이로스', section: 'bottom' },
  { id: 'settings', label: 'Settings', labelKo: '설정', section: 'bottom' },
];

// SVG icons (no emoji)
function Icon({ name, size = 18 }: { name: string; size?: number }) {
  const s = `${size}`;
  const props = { width: s, height: s, viewBox: "0 0 24 24", fill: "none", stroke: "currentColor", strokeWidth: "1.5", strokeLinecap: "round" as const, strokeLinejoin: "round" as const };
  switch (name) {
    case 'chat': return <svg {...props}><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>;
    case 'files': return <svg {...props}><path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/></svg>;
    case 'routes': return <svg {...props}><circle cx="18" cy="18" r="3"/><circle cx="6" cy="6" r="3"/><path d="M13 6h3a2 2 0 0 1 2 2v7"/><path d="M6 9v12"/></svg>;
    case 'costs': return <svg {...props}><line x1="12" y1="1" x2="12" y2="23"/><path d="M17 5H9.5a3.5 3.5 0 0 0 0 7h5a3.5 3.5 0 0 1 0 7H6"/></svg>;
    case 'kairos': return <svg {...props}><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg>;
    case 'memory': return <svg {...props}><path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/></svg>;
    case 'team': return <svg {...props}><path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M23 21v-2a4 4 0 0 0-3-3.87"/><path d="M16 3.13a4 4 0 0 1 0 7.75"/></svg>;
    case 'settings': return <svg {...props}><circle cx="12" cy="12" r="3"/><path d="M12 1v2M12 21v2M4.22 4.22l1.42 1.42M18.36 18.36l1.42 1.42M1 12h2M21 12h2M4.22 19.78l1.42-1.42M18.36 5.64l1.42-1.42"/></svg>;
    case 'lang': return <svg {...props}><circle cx="12" cy="12" r="10"/><line x1="2" y1="12" x2="22" y2="12"/><path d="M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z"/></svg>;
    case 'sun': return <svg {...props}><circle cx="12" cy="12" r="5"/><line x1="12" y1="1" x2="12" y2="3"/><line x1="12" y1="21" x2="12" y2="23"/><line x1="4.22" y1="4.22" x2="5.64" y2="5.64"/><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"/><line x1="1" y1="12" x2="3" y2="12"/><line x1="21" y1="12" x2="23" y2="12"/></svg>;
    case 'moon': return <svg {...props}><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/></svg>;
    default: return <span className="text-xs">{name[0]}</span>;
  }
}

interface Props {
  active: string;
  onNavigate: (id: string) => void;
  onThemeToggle: () => void;
  theme: 'dark' | 'light';
}

export function ActivityBar({ active, onNavigate, onThemeToggle, theme }: Props) {
  const ko = getLang() === 'ko';
  const top = items.filter(i => i.section === 'top');
  const bottom = items.filter(i => i.section === 'bottom');

  return (
    <div className="w-12 bg-[var(--color-surface)] border-r border-[var(--color-border)] flex flex-col items-center py-2 shrink-0 h-[calc(100vh-24px)] sticky top-0">
      <div className="w-7 h-7 rounded-md bg-gradient-to-br from-[var(--color-accent)] to-purple-400 flex items-center justify-center text-white text-[10px] font-bold mb-4">A</div>

      <div className="flex-1 flex flex-col items-center gap-0.5">
        {top.map((item) => (
          <button key={item.id} onClick={() => onNavigate(item.id)} title={ko ? item.labelKo : item.label}
            className={`w-10 h-10 rounded-lg flex items-center justify-center transition-all ${active === item.id ? 'bg-[var(--color-accent)]/15 text-[var(--color-accent)]' : 'text-[var(--color-text2)] hover:bg-[var(--color-surface2)] hover:text-[var(--color-text)]'}`}>
            <Icon name={item.id} />
          </button>
        ))}
      </div>

      <div className="flex flex-col items-center gap-0.5 mb-1">
        {bottom.map((item) => (
          <button key={item.id} onClick={() => onNavigate(item.id)} title={ko ? item.labelKo : item.label}
            className={`w-10 h-10 rounded-lg flex items-center justify-center transition-all ${active === item.id ? 'bg-[var(--color-accent)]/15 text-[var(--color-accent)]' : 'text-[var(--color-text2)] hover:bg-[var(--color-surface2)] hover:text-[var(--color-text)]'}`}>
            <Icon name={item.id} />
          </button>
        ))}
        <button onClick={onThemeToggle} title="Theme" className="w-10 h-10 rounded-lg flex items-center justify-center text-[var(--color-text2)] hover:bg-[var(--color-surface2)]">
          <Icon name={theme === 'dark' ? 'sun' : 'moon'} />
        </button>
      </div>
    </div>
  );
}
