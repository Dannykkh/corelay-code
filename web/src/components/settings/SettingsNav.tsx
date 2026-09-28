/**
 * Every section the settings surface knows about. Sections not yet ported from
 * the previous UI stay in the union so cross-section navigation props type-check.
 * The rendered navigation lives in pages/RuntimeSettings.tsx.
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
  | 'gateway'
  | 'memory'
  | 'browser-storage'
  | 'privacy'
  | 'appearance'
  | 'shortcuts'
  | 'advanced';
