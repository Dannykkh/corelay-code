import { useState } from 'react';
import { SettingsPage as QuickStartSettings } from './Settings';
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
import type { SettingsSection } from '@/components/settings/SettingsNav';
import { cn } from '@/lib/utils';

type Tab = 'quickstart' | SettingsSection;

interface TabGroup {
  group: string;
  items: { id: Tab; label: string }[];
}

/** Runtime first: the scheduler is the reason this proxy exists. */
const TAB_GROUPS: TabGroup[] = [
  {
    group: 'Runtime',
    items: [
      { id: 'quickstart', label: 'Quick Start' },
      { id: 'overview', label: 'Overview' },
      { id: 'scheduler', label: 'Scheduler' },
      { id: 'accounts', label: 'Accounts' },
      { id: 'providers', label: 'Providers' },
      { id: 'routing', label: 'Routing' },
    ],
  },
  {
    group: 'Harness',
    items: [
      { id: 'agents', label: 'Agents' },
      { id: 'commands', label: 'Commands' },
      { id: 'skills', label: 'Skills' },
      { id: 'loops', label: 'Loops' },
      { id: 'verification', label: 'Verification' },
      { id: 'handoffs', label: 'Handoffs' },
    ],
  },
  {
    group: 'Data',
    items: [
      { id: 'memory', label: 'Memory' },
      { id: 'privacy', label: 'Privacy' },
      { id: 'advanced', label: 'Advanced' },
    ],
  },
];

const ALL_TABS = TAB_GROUPS.flatMap((group) => group.items);

/**
 * Settings surface. `quickstart` is the original provider/model/language panel;
 * the rest are the runtime plane and harness controls.
 */
export function SettingsPage() {
  const [tab, setTab] = useState<Tab>('quickstart');

  // Ported sections can link to each other; ignore targets not rendered yet
  // rather than switching to a blank tab.
  const navigate = (section: SettingsSection) => {
    if (ALL_TABS.some((item) => item.id === section)) setTab(section);
  };

  return (
    <div className="flex-1 overflow-y-auto">
      <nav
        aria-label="Settings sections"
        className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-surface-800 px-6 py-3"
      >
        {TAB_GROUPS.map((group) => (
          <div key={group.group} className="flex flex-wrap items-center gap-1">
            <span className="mr-1 text-[10px] uppercase tracking-wide text-surface-500">
              {group.group}
            </span>
            {group.items.map((item) => {
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
        ))}
      </nav>

      {tab === 'quickstart' ? (
        <QuickStartSettings />
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
          {tab === 'memory' && <MemorySettings />}
          {tab === 'privacy' && <PrivacySettings />}
          {tab === 'advanced' && <AdvancedSettings />}
        </div>
      )}
    </div>
  );
}
