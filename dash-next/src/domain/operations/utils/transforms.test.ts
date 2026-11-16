import { describe, expect, it } from 'vitest';

import { toTarget } from '../testing/fixtures';
import { toTargetRow } from './transforms';

describe('toTargetRow', () => {
  it('shows the facts of a target', () => {
    expect(
      toTargetRow(toTarget({ committed: true, completedAt: '2026-11-13T12:00:00Z' }), 0),
    ).toEqual({
      id: 'child-1',
      cluster: 'prod',
      node: 'node-1',
      state: 'running',
      outcome: 'unknown',
      committed: 'yes',
      completed: new Date('2026-11-13T12:00:00Z').toLocaleString(),
    });
  });

  it('shows placeholders for missing facts', () => {
    expect(toTargetRow({}, 3)).toEqual({
      id: '3',
      cluster: '—',
      node: '—',
      state: '—',
      outcome: '',
      committed: 'no',
      completed: '—',
    });
  });

  it('shows an unparseable completion time as received', () => {
    expect(toTargetRow(toTarget({ completedAt: 'soon' }), 0).completed).toBe('soon');
  });
});
