import { heldLines } from './constants';
import type { ExecState } from './types';

export const afterOpen = (): ExecState => 'connected';

export const afterClose = (state: ExecState): ExecState =>
  state === 'connected' || state === 'closed' ? 'closed' : 'error';

export const withLines = (held: string[], added: string[], limit: number = heldLines): string[] =>
  [...held, ...added].slice(-limit);
