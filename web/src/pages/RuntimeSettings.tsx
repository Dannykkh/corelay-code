import { useState } from 'react';
import { ChevronDown, ChevronRight } from 'lucide-react';
import { SettingsPage as QuickStartSettings } from './Settings';
import { MemoryPage } from './Memory';
import { OverviewSettings } from '@/components/settings/OverviewSettings';
import { SchedulerSettings } from '@/components/settings/SchedulerSettings';
import { AccountsSettings } from '@/components/settings/AccountsSettings';
import { ProvidersSettings } from '@/components/settings/ProvidersSettings';
import { RoutingSettings } from '@/components/settings/RoutingSettings';
import { AgentsSettings } from '@/components/settings/AgentsSettings';
import { CommandsSettings } from '@/components/settings/CommandsSettings';
import { SkillsSettings } from '@/components/settings/SkillsSettings';
import { LoopsSettings } from '@/components/settings/LoopsSettings';
import { VerificationSettings } from '@/components/settings/VerificationSettings';
import { HandoffsSettings } from '@/components/settings/HandoffsSettings';
import { MemorySettings } from '@/components/settings/MemorySettings';
import { PrivacySettings } from '@/components/settings/PrivacySettings';
import { AdvancedSettings } from '@/components/settings/AdvancedSettings';
import { PermissionsSettings } from '@/components/settings/PermissionsSettings';
import { GatewaySettings } from '@/components/settings/GatewaySettings';
import type { SettingsSection } from '@/components/settings/SettingsNav';
import { cn } from '@/lib/utils';

type Tab = 'quickstart' | SettingsSection;

interface TabGroup {
  group: string;
  /** Collapsed until opened, unless the current tab lives in the group. */
  collapsible?: boolean;
  items: { id: Tab; label: string }[];
}

/** Settings organized into 4 groups (docs/plan/ui-ux-architecture-overhaul.md §7). */
const TAB_GROUPS: TabGroup[] = [
  {
    group: '모델·제공자',
    items: [
      { id: 'quickstart', label: 'Quick Start' },
      { id: 'overview', label: 'Overview' },
      { id: 'accounts', label: 'Accounts' },
      { id: 'providers', label: 'Providers' },
      { id: 'scheduler', label: 'Scheduler' },
      { id: 'routing', label: 'Routing' },
    ],
  },
  {
    group: '안전·권한',
    items: [
      { id: 'permissions', label: 'Permissions' },
      { id: 'privacy', label: 'Privacy' },
      { id: 'gateway', label: 'Team Gateway' },
    ],
  },
  {
    group: '일반·데이터',
    items: [
      { id: 'memory', label: 'Server Memory' },
      { id: 'browser-storage', label: 'Browser Storage' },
      { id: 'advanced', label: 'Advanced' },
    ],
  },
  {
    group: '하네스',
    collapsible: true,
    items: [
      { id: 'agents', label: 'Agents' },
      { id: 'commands', label: 'Commands' },
      { id: 'skills', label: 'Skills' },
      { id: 'loops', label: 'Loops' },
      { id: 'verification', label: 'Verification' },
      { id: 'handoffs', label: 'Handoffs' },
    ],
  },
];

const ALL_TABS = TAB_GROUPS.flatMap((group) => group.items);

export interface SettingsPageProps {
  initialTab?: Tab;
}

/**
 * Settings surface. `quickstart` is the original provider/model/language panel;
 * the rest are the runtime plane and harness controls.
 */
export function SettingsPage({ initialTab = 'quickstart' }: SettingsPageProps = {}) {
  const [tab, setTab] = useState<Tab>(initialTab);
  const [openGroups, setOpenGroups] = useState<Record<string, boolean>>(() => Object.fromEntries(
    TAB_GROUPS.filter((g) => g.collapsible).map((g) => [g.group, g.items.some((item) => item.id === initialTab)]),
  ));

  // Ported sections can link to each other; ignore targets not rendered yet
  // rather than switching to a blank tab.
  const navigate = (section: SettingsSection) => {
    if (!ALL_TABS.some((item) => item.id === section)) return;
    const group = TAB_GROUPS.find((g) => g.collapsible && g.items.some((item) => item.id === section));
    if (group) setOpenGroups((prev) => ({ ...prev, [group.group]: true }));
    setTab(section);
  };

  return (
    <div className="flex-1 overflow-y-auto">
      <nav
        aria-label="Settings sections"
        className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-surface-800 px-6 py-3"
      >
        {TAB_GROUPS.map((group) => {
          const open = !group.collapsible || Boolean(openGroups[group.group]);
          return (
          <div key={group.group} className="flex flex-wrap items-center gap-1">
            {group.collapsible ? (
              <button
                type="button"
                onClick={() => setOpenGroups((prev) => ({ ...prev, [group.group]: !open }))}
                aria-expanded={open}
                className="mr-1 inline-flex items-center gap-1 text-[10px] uppercase tracking-wide text-surface-500 hover:text-surface-200"
              >
                {open ? <ChevronDown aria-hidden="true" className="w-3 h-3" /> : <ChevronRight aria-hidden="true" className="w-3 h-3" />}
                {group.group}
                {!open && <span className="normal-case tracking-normal">({group.items.length})</span>}
              </button>
            ) : (
              <span className="mr-1 text-[10px] uppercase tracking-wide text-surface-500">
                {group.group}
              </span>
            )}
            {open && group.items.map((item) => {
              const selected = item.id === tab;
              return (
                <button
                  key={item.id}
                  type="button"
                  onClick={() => setTab(item.id)}
                  aria-current={selected ? 'page' : undefined}
                  className={cn(
                    'rounded-md px-2.5 py-1.5 text-xs transition-colors',
                    selected
                      ? 'bg-brand-500/15 text-brand-400'
                      : 'text-surface-400 hover:bg-surface-900 hover:text-surface-200',
                  )}
                >
                  {item.label}
                </button>
              );
            })}
          </div>
          );
        })}
      </nav>

      {tab === 'quickstart' ? (
        <QuickStartSettings />
      ) : tab === 'memory' ? (
        <div className="w-full">
          <MemoryPage />
        </div>
      ) : (
        <div className="mx-auto max-w-3xl px-6 py-5">
          {tab === 'overview' && <OverviewSettings onNavigate={navigate} />}
          {tab === 'scheduler' && <SchedulerSettings />}
          {tab === 'accounts' && <AccountsSettings />}
          {tab === 'providers' && <ProvidersSettings />}
          {tab === 'routing' && <RoutingSettings />}
          {tab === 'agents' && <AgentsSettings />}
          {tab === 'commands' && <CommandsSettings />}
          {tab === 'skills' && <SkillsSettings />}
          {tab === 'loops' && <LoopsSettings />}
          {tab === 'verification' && <VerificationSettings />}
          {tab === 'handoffs' && <HandoffsSettings />}
          {tab === 'permissions' && <PermissionsSettings />}
          {tab === 'privacy' && <PrivacySettings />}
          {tab === 'gateway' && <GatewaySettings />}
          {tab === 'browser-storage' && <MemorySettings />}
          {tab === 'advanced' && <AdvancedSettings />}
        </div>
      )}
    </div>
  );
}
