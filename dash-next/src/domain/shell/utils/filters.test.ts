import { describe, expect, it } from 'vitest';

import type { PaletteEntry } from '../types/screen';
import { matchingQuery } from './filters';

const entries: PaletteEntry[] = [
  { id: 'pods', group: 'Navigate', title: 'Pods', subtitle: 'Pod', href: '/c/a/pods' },
  { id: 'nodes', group: 'Navigate', title: 'Nodes', subtitle: 'Capacity', href: '/c/a/nodes' },
  { id: 'cluster:edge', group: 'Switch cluster', title: 'edge', href: '/c/edge/overview' },
];

const idsMatching = (query: string): string[] => matchingQuery(entries, query).map(({ id }) => id);

describe('matchingQuery', () => {
  it('shows everything for a blank query', () => {
    expect([idsMatching(''), idsMatching('   ')]).toEqual([
      ['pods', 'nodes', 'cluster:edge'],
      ['pods', 'nodes', 'cluster:edge'],
    ]);
  });

  it('matches title, subtitle and group name, ignoring case and order kept', () => {
    expect([idsMatching('POD'), idsMatching('capacity'), idsMatching('switch')]).toEqual([
      ['pods'],
      ['nodes'],
      ['cluster:edge'],
    ]);
  });

  it('ignores spaces around the query', () => {
    expect(idsMatching('  pods ')).toEqual(['pods']);
  });

  it('matches nothing when nothing contains the query', () => {
    expect(idsMatching('zzz')).toEqual([]);
  });
});
