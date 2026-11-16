import type { PodTab, ShellPhase, TabMode } from '../types/terminal';

export const hasPane = (
  { started }: Pick<PodTab, 'started'>,
  mode: TabMode,
  canWrite: boolean,
): boolean =>
  mode === 'exec' ? canWrite && started.includes('exec') : started.includes('logs') || !canWrite;

export const isAwaited = (phase: ShellPhase): boolean =>
  phase.type === 'creating' || phase.type === 'waiting';

export const matchesFilter = (line: string, filter: string): boolean =>
  line.toLowerCase().includes(filter.toLowerCase());
