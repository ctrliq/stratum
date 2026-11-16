import { defaultView, views } from '../constants';
import type { ClustersView } from '../types';

const separator = '=';

const isPlacementLine = (line: string): boolean => line.indexOf(separator) > 0;

const fromPlacementLine = (line: string): [key: string, value: string] => {
  const at = line.indexOf(separator);
  return [line.slice(0, at).trim(), line.slice(at + 1).trim()];
};

export const fromPlacementText = (text: string): Record<string, string> | undefined => {
  const lines = text
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '' && !line.startsWith('#'));
  return lines.every(isPlacementLine)
    ? Object.fromEntries(lines.map(fromPlacementLine))
    : undefined;
};

export const fromChoice = <TChoice extends string>(
  choices: readonly TChoice[],
  value: unknown,
  fallback: TChoice,
): TChoice => choices.find((choice) => choice === value) ?? fallback;

export const fromStoredView = (stored: string | null): ClustersView =>
  fromChoice(views, stored, defaultView);
