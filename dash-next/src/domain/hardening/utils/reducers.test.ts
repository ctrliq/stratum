import { describe, expect, it } from 'vitest';

import { allFilters } from '../constants';
import type { FindingsWalk, ReportDetailView } from '../types/engineState';
import { toEmptyWalk, withOpenedFinding, withPage, withRetained } from './reducers';

const empty = toEmptyWalk({ reportGeneration: 'g-7', filters: allFilters });
const subject = { scope: 'node', nodeUID: 'uid-1', component: 'kubelet', key: 'serving' };

const toPage = (page: ReportDetailView): ReportDetailView => ({
  findings: [],
  observations: [],
  totalFindings: 3,
  complete: true,
  state: 'available',
  ...page,
});

const walked = (pages: ReportDetailView[]): FindingsWalk => pages.reduce(withPage, empty);

describe('withPage', () => {
  it('keeps the findings of all pages in the order served and follows the cursor', () => {
    const walk = walked([
      toPage({ findings: [{ id: 'a' }], nextCursor: 'c1' }),
      toPage({ findings: [{ id: 'b' }], nextCursor: 'c2' }),
    ]);
    expect([walk.findings, walk.nextCursor, walk.issuedCursors]).toEqual([
      [{ id: 'a' }, { id: 'b' }],
      'c2',
      ['c1', 'c2'],
    ]);
  });

  it.each([[undefined], [null], ['']])('ends the walk on the cursor %s', (nextCursor) => {
    expect(withPage(empty, toPage({ nextCursor })).nextCursor).toBeUndefined();
  });

  it('takes the total, the completeness and the state from the last page', () => {
    const walk = walked([
      toPage({ totalFindings: 9, complete: false, state: 'partial', nextCursor: 'c1' }),
      toPage({ totalFindings: 12, complete: true, state: 'complete' }),
    ]);
    expect([walk.totalFindings, walk.isComplete, walk.state]).toEqual([12, true, 'complete']);
  });

  it('falls back safely when the last page omits its totals', () => {
    const walk = withPage(empty, {
      findings: [{ id: 'a' }, { id: 'b' }],
    });
    expect([walk.totalFindings, walk.isComplete, walk.state]).toEqual([2, false, undefined]);
  });

  it('keeps an observation repeated on several pages once, as the later page stated it', () => {
    const walk = walked([
      toPage({
        observations: [
          { signal: 'certificate', subject, availability: 'unavailable' },
          { signal: 'version', subject },
        ],
        nextCursor: 'c1',
      }),
      toPage({ observations: [{ signal: 'certificate', subject, availability: 'available' }] }),
    ]);
    expect(walk.observations).toEqual([
      { signal: 'certificate', subject, availability: 'available' },
      { signal: 'version', subject },
    ]);
  });

  it('stops on a cursor the hub already issued and labels the findings as not the whole set', () => {
    const walk = walked([
      toPage({ findings: [{ id: 'a' }], nextCursor: 'c1' }),
      toPage({ findings: [{ id: 'b' }], nextCursor: 'c1' }),
    ]);
    expect([walk.nextCursor, walk.isComplete, walk.findings.length]).toEqual([undefined, false, 2]);
  });

  it('stops after 100 pages', () => {
    const pages = Array.from({ length: 100 }, (_unused, index) =>
      toPage({ findings: [{ id: String(index) }], nextCursor: `c${index}` }),
    );
    expect([
      walked(pages.slice(0, 99)).nextCursor,
      walked(pages).nextCursor,
      walked(pages).isComplete,
      walked(pages).findings.length,
    ]).toEqual(['c98', undefined, false, 100]);
  });
});

describe('withOpenedFinding', () => {
  const certificate = { signal: 'certificate', subject, id: 'o-1' };
  const walk: FindingsWalk = {
    ...empty,
    observations: [certificate, { signal: 'version', subject }],
  };
  const finding = { signal: 'certificate', subject, severity: 'warning', evidenceRef: 'ev-1' };
  const opened = withOpenedFinding(undefined, finding, walk);

  it('opens the evidence with a snapshot of what was walked', () => {
    expect(opened).toEqual({
      evidenceRef: 'ev-1',
      reportGeneration: 'g-7',
      signal: 'certificate',
      severity: 'warning',
      observation: certificate,
      certificateObservations: [certificate],
    });
  });

  it('switches only the severity for another finding sharing the open evidence', () => {
    const later = { ...walk, observations: [] };
    expect(withOpenedFinding(opened, { evidenceRef: 'ev-1', severity: 'critical' }, later)).toEqual(
      { ...opened, severity: 'critical' },
    );
  });

  it('starts clean for another reference or once the report moved', () => {
    const moved = { ...walk, reportGeneration: 'g-8', observations: [] };
    expect([
      withOpenedFinding(opened, { evidenceRef: 'ev-2' }, walk)?.evidenceRef,
      withOpenedFinding(opened, finding, moved),
    ]).toEqual([
      'ev-2',
      { ...opened, reportGeneration: 'g-8', observation: undefined, certificateObservations: [] },
    ]);
  });

  it('opens nothing for a finding without an evidence reference', () => {
    expect(withOpenedFinding(undefined, { evidenceRef: null }, walk)).toBeUndefined();
  });
});

describe('withRetained', () => {
  const held = { scope: 'prod', value: empty };

  it('keeps the last walk of a cluster while a newer read of it has no answer', () => {
    expect(withRetained(held, 'prod', undefined)).toBe(held);
  });

  it('takes the newer walk of the same cluster', () => {
    const newer = { ...empty, totalFindings: 4 };
    expect(withRetained(held, 'prod', newer)).toEqual({ scope: 'prod', value: newer });
  });

  it('holds on to an unchanged walk without a new state', () => {
    expect(withRetained(held, 'prod', empty)).toBe(held);
  });

  it.each([['staging'], [undefined]])(
    'drops the walk of the previous cluster once the cluster is %s',
    (scope) => {
      expect(withRetained(held, scope, undefined)).toEqual({ scope, value: undefined });
    },
  );
});
