import { describe, expect, it } from 'vitest';

import { allFilters } from '../constants';
import type { FindingsWalk } from '../types/engineState';
import {
  toAttemptLine,
  toBackupCaption,
  toCauseLine,
  toCertificateFacts,
  toChildLine,
  toClusterListPath,
  toCoarseDuration,
  toControlBadge,
  toCopyAge,
  toDrilldownTitle,
  toElapsed,
  toEngineStatePath,
  toEvidenceRefusal,
  toExpiryLine,
  toExpiryTone,
  toFindingDetail,
  toFindingsHeader,
  toFindingsRefusal,
  toGroupHeading,
  toListingLine,
  toMirrorPodLine,
  toObservationFacts,
  toObservedCaption,
  toOperatorDetail,
  toOperatorLabel,
  toPartialEvidenceNote,
  toPartialFindingsNote,
  toPolicyLines,
  toPreviousEvidenceNote,
  toPreviousReportNote,
  toProfileTitle,
  toRawItemTitle,
  toReasonText,
  toRenewalFacts,
  toRetainedFiltersNote,
  toRoleCoverageText,
  toRuntimeLine,
  toSampleLine,
  toSeverityText,
  toSourceFacts,
  toSummaryRefusal,
  toSupervisorFacts,
  toUptimeLine,
  toVerdictWord,
  toWarningText,
} from './formatters';

const now = Date.UTC(2026, 0, 10, 12);
const second = 1000;
const minute = 60 * second;
const hour = 60 * minute;
const day = 24 * hour;
const at = (offset: number): string => new Date(now + offset).toISOString();
const local = (time: string): string => new Date(time).toLocaleString();

const walk: FindingsWalk = {
  reportGeneration: 'g-7',
  filters: allFilters,
  findings: Array.from({ length: 7 }, (_unused, index) => ({ id: String(index) })),
  observations: [],
  totalFindings: 12,
  isComplete: false,
  isCut: false,
  state: 'complete',
  issuedCursors: [],
  nextCursor: undefined,
};

const refusal = { status: 503, message: 'report incomplete', isRetryable: true };

describe('toElapsed', () => {
  it.each([
    [-20 * second, '20s'],
    [-(59 * minute + 59 * second), '59m'],
    [-23 * hour, '23h'],
    [-24 * hour, '1d'],
    [5 * second, undefined],
  ])('reads a time %s ms from now as %s', (offset, expected) => {
    expect(toElapsed(at(offset), now)).toBe(expected);
  });

  it('has no age for a missing or unparsable time', () => {
    expect([toElapsed(undefined, now), toElapsed('soon', now)]).toEqual([undefined, undefined]);
  });
});

describe('toCoarseDuration', () => {
  it.each([
    [0, '0s'],
    [59, '59s'],
    [60, '1m'],
    [240, '4m'],
    [3599, '59m'],
    [14640, '4h'],
    [172799, '47h'],
    [172800, '2d'],
    [604800, '7d'],
  ])('reads %s seconds as %s', (seconds, text) => {
    expect(toCoarseDuration(seconds)).toBe(text);
  });
});

describe('toOperatorLabel and toOperatorDetail', () => {
  it.each([
    ['active', 'Active'],
    ['inactive', 'Inactive'],
    ['missing', 'Inactive'],
    ['unknown', 'Unknown'],
    ['draining', 'Unknown'],
  ])('reads the verdict %s as %s', (state, label) => {
    expect(toOperatorLabel(state)).toBe(label);
  });

  it.each([
    ['active', false, 'operator lease held'],
    ['inactive', false, 'operator lease stale or held by nobody'],
    ['missing', false, 'no operator lease found'],
    ['unknown', true, "unknown: this cluster's credential cannot read the operator lease"],
    [
      'unknown',
      false,
      'no verdict yet — the cluster has not reported the operator lease, or the read failed',
    ],
    ['draining', false, 'no verdict from state "draining" — a newer hub\'s value'],
  ])('explains the verdict %s (refused: %s)', (state, isRefused, detail) => {
    expect(toOperatorDetail({ state, holder: '—', isRefused })).toBe(detail);
  });
});

describe('toObservedCaption', () => {
  it('states when the hub observed the facts, when it says so', () => {
    expect([toObservedCaption(at(0)), toObservedCaption(undefined)]).toEqual([
      `live · cluster-visible signals · observed ${new Date(now).toLocaleTimeString()}`,
      'live · cluster-visible signals',
    ]);
  });
});

describe('the control profile texts', () => {
  it('title the card, head each group and name the badge', () => {
    expect([
      toProfileTitle(24),
      toGroupHeading({
        group: 'API Server',
        sources: ['kube-apiserver.yaml', 'admission-webhook.yaml'],
        controls: [],
      }),
      toControlBadge('cis'),
      toControlBadge('exception'),
      toControlBadge('other'),
      toControlBadge(undefined),
    ]).toEqual([
      'CIS control profile (24)',
      'API Server · kube-apiserver.yaml, admission-webhook.yaml',
      '--profile cis',
      'exception',
      undefined,
      undefined,
    ]);
  });
});

describe('toEngineStatePath', () => {
  it('addresses the drill-down of a cluster, escaping its id', () => {
    expect([toEngineStatePath('prod'), toEngineStatePath('site a/b')]).toEqual([
      '/clusters/prod/engine-state',
      '/clusters/site%20a%2Fb/engine-state',
    ]);
  });

  it("carries the cluster list's search text, so closing can hand it back", () => {
    expect(toEngineStatePath('prod', '?q=eu')).toBe('/clusters/prod/engine-state?q=eu');
  });
});

describe('toClusterListPath', () => {
  it('returns to the cluster list with the search text it was left with', () => {
    expect([toClusterListPath('?q=eu'), toClusterListPath('')]).toEqual([
      '/clusters?q=eu',
      '/clusters',
    ]);
  });
});

describe('the summary texts', () => {
  it('title the panel by name, falling back to the id', () => {
    expect([toDrilldownTitle('prod', 'c-1'), toDrilldownTitle(undefined, 'c-1')]).toEqual([
      'Engine state — prod',
      'Engine state — c-1',
    ]);
  });

  it('quote a refused summary read', () => {
    const refused = { ...refusal, message: 'hub overloaded' };
    expect([toSummaryRefusal(refused, false), toSummaryRefusal(refused, true)]).toEqual([
      'engine state unavailable — hub overloaded',
      'the latest read was refused — hub overloaded',
    ]);
  });
});

describe('the findings texts', () => {
  it('head the list with the total and the state', () => {
    expect([toFindingsHeader(walk), toFindingsHeader({ ...walk, state: undefined })]).toEqual([
      '12 findings · complete',
      '12 findings · —',
    ]);
  });

  it('label an incomplete walk and findings of an older report', () => {
    expect([
      toPartialFindingsNote(walk),
      toPreviousReportNote('g-7', 'g-8'),
      toPreviousReportNote('g-7', undefined),
    ]).toEqual([
      'partial — 7 of 12 findings served',
      'these findings are from the previous report — g-7; the summary describes g-8.',
      'these findings are from the previous report — g-7; the summary describes a report without retained detail.',
    ]);
  });

  it.each([
    [
      { status: 409, currentReportGeneration: 'g-8' },
      'the report changed while reading — the generation moved to g-8; these findings are from the previous report.',
    ],
    [
      { status: 409 },
      'the report changed while reading; these findings are from the previous report.',
    ],
    [
      { status: 410, retainedFrom: 'g-3', retainedTo: 'g-7' },
      'report detail expired — retained g-3 to g-7; the summary above stays current.',
    ],
    [
      { status: 410, retainedFrom: 'g-3' },
      'report detail expired — retained g-3 to —; the summary above stays current.',
    ],
    [{ status: 410 }, 'report detail expired; the summary above stays current.'],
    [{ status: 503 }, 'findings unavailable — report incomplete · retryable'],
    [
      { status: 403, message: 'insufficient role', isRetryable: false },
      'findings unavailable — insufficient role',
    ],
    [
      { status: 'unknown' as const, message: 'the engine-state request timed out' },
      'findings unavailable — the engine-state request timed out · retryable',
    ],
  ])('explain the refusal %o', (patch, note) => {
    expect(toFindingsRefusal({ ...refusal, ...patch })).toBe(note);
  });

  it('name the filters the retained findings were loaded under', () => {
    expect([
      toRetainedFiltersNote({ ...allFilters, signal: 'certificate' }, true),
      toRetainedFiltersNote({ signal: '', nodeUID: 'uid-1', minimumSeverity: 'warning' }, false),
    ]).toEqual([
      'retained findings for certificate · all nodes · all severities; the selected filters are loading.',
      'retained findings for all signals · uid-1 · minimum severity warning; the selected filters could not be loaded.',
    ]);
  });

  it('state a finding as the hub stated it', () => {
    const subject = { scope: 'node', nodeUID: 'uid-1', component: 'kubelet', key: 'serving' };
    expect([
      toFindingDetail({ signal: 'certificate', subject, sampledAt: at(0) }),
      toFindingDetail({}),
      toFindingDetail({ signal: 'version', subject: { scope: 'cluster' }, sampledAt: null }),
      toSeverityText({ severity: 'critical' }),
      toSeverityText({}),
    ]).toEqual([
      `certificate · node · node uid-1 · kubelet · serving · sampled ${local(at(0))}`,
      'unknown signal · unknown scope · sampled —',
      'version · cluster · sampled —',
      'critical',
      'unknown',
    ]);
  });

  it('quote an observation reason', () => {
    expect([
      toReasonText({ code: 'timeout', message: 'runtime did not answer', retryable: true }),
      toReasonText({ code: 'denied', message: 'no access' }),
    ]).toEqual(['timeout: runtime did not answer · retryable', 'denied: no access']);
  });
});

describe('the evidence texts', () => {
  it.each([
    ['detail expired', false, 'evidence unavailable — detail expired'],
    ['source not connected', true, 'evidence unavailable — source not connected · retryable'],
  ])('quote the refusal %s', (message, isRetryable, note) => {
    expect(toEvidenceRefusal({ status: 410, message, isRetryable })).toBe(note);
  });

  it('label a partial fragment and evidence of an older report', () => {
    expect([
      toPartialEvidenceNote({ items: [{}, {}], totalItems: 5 }),
      toPreviousEvidenceNote('g-7'),
      toRawItemTitle('version'),
      toRawItemTitle(undefined),
    ]).toEqual([
      'partial — 2 of 5 items served',
      'this evidence is from the previous report — g-7.',
      'evidence item · version',
      'evidence item · —',
    ]);
  });

  it('print the source as recorded, with dashes for missing facts', () => {
    const startedAt = at(-3 * hour);
    expect([
      toSourceFacts({ processInstance: 'p-1', build: 'b-1', producer: 'engine', startedAt }, now),
      toSourceFacts({ processInstance: 'p-1', producer: 'engine' }, now),
    ]).toEqual([
      [
        { label: 'Process', value: 'p-1' },
        { label: 'Build', value: 'b-1' },
        { label: 'Producer', value: 'engine' },
        { label: 'Started', value: `${local(startedAt)} (3h ago)` },
      ],
      [
        { label: 'Process', value: 'p-1' },
        { label: 'Build', value: '—' },
        { label: 'Producer', value: 'engine' },
        { label: 'Started', value: '—' },
      ],
    ]);
  });
});

describe('the supervisor texts', () => {
  it('list the process and build', () => {
    expect(toSupervisorFacts({ processID: 4242 })).toEqual([
      { label: 'Process', value: '4242' },
      { label: 'Build', value: '—' },
    ]);
  });

  it('state an uptime only for an actual start', () => {
    const started = at(-2 * hour);
    expect([
      toUptimeLine({ processStartedAt: started }, now),
      toUptimeLine({}, now),
      toUptimeLine({ processStartedAt: 'soon' }, now),
      toUptimeLine({ processStartedAt: at(hour) }, now),
    ]).toEqual([`uptime 2h · started ${local(started)}`, undefined, undefined, undefined]);
  });

  it.each([
    [true, true, 'etcd · desired · running'],
    [true, false, 'etcd · desired · not running'],
    [false, true, 'etcd · not desired · running'],
    [false, false, 'etcd · not desired · not running'],
    [undefined, true, 'etcd · — · running'],
    [true, undefined, 'etcd · desired · —'],
    [undefined, undefined, 'etcd · — · —'],
  ])('state a child desired %s and running %s as "%s"', (desired, running, line) => {
    expect(toChildLine({ component: 'etcd', desired, running })).toBe(line);
  });

  it('state a child ownership and a missing name as recorded', () => {
    expect(toChildLine({ desired: true, running: false, ownership: 'foreign' })).toBe(
      '— · desired · not running · foreign',
    );
  });

  it('label the runtime and the mirror-Pod observations', () => {
    const reason = { code: 'timeout', message: 'runtime did not answer', retryable: true };
    expect([
      toRuntimeLine({ availability: 'available', sampledAt: at(-40 * second) }, now),
      toRuntimeLine({ availability: 'unavailable', sampledAt: at(-40 * second), reason }, now),
      toMirrorPodLine({ availability: 'unavailable', sampledAt: at(-2 * minute) }, now),
      toRuntimeLine({}, now),
      toMirrorPodLine({}, now),
    ]).toEqual([
      'runtime observation: available · sampled 40s ago',
      'runtime observation: unavailable · sampled 40s ago · timeout: runtime did not answer · retryable',
      'Kubernetes observation — mirror Pod: unavailable · sampled 2m ago',
      'runtime observation: — · sampled —',
      'Kubernetes observation — mirror Pod: — · sampled —',
    ]);
  });
});

describe('the certificate texts', () => {
  it('sum up a role', () => {
    expect(
      toRoleCoverageText({
        role: 'kubelet-serving',
        expected: 3,
        evaluated: 2,
        unknown: 1,
        notApplicable: 3,
      }),
    ).toBe('expected 3 · evaluated 2 · unknown 1 · not applicable 3');
  });

  it.each([
    [-second, '{time} · expired'],
    [0, '{time} · expired'],
    [second, '{time} · 0d 0h 0m 1s left · critical'],
    [7 * day - second, '{time} · 6d 23h 59m 59s left · critical'],
    [7 * day, '{time} · 7d 0h 0m 0s left · warning'],
    [20 * day, '{time} · 20d 0h 0m 0s left · warning'],
    [30 * day - second, '{time} · 29d 23h 59m 59s left · warning'],
    [30 * day, '{time} · 30d 0h 0m 0s left · healthy'],
    [400 * day, '{time} · 400d 0h 0m 0s left · healthy'],
  ])('band an expiry %s ms from now', (offset, line) => {
    expect(toExpiryLine(at(offset), now)).toBe(
      `notAfter ${line.replace('{time}', local(at(offset)))}`,
    );
  });

  it.each([
    [-second, 'error'],
    [second, 'error'],
    [7 * day, 'warning'],
    [30 * day, 'success'],
  ])('style an expiry %s ms from now as %s', (offset, tone) => {
    expect(toExpiryTone(at(offset), now)).toBe(tone);
  });

  it('invent no expiry line', () => {
    expect([toExpiryLine(undefined, now), toExpiryLine('soon', now)]).toEqual([
      undefined,
      undefined,
    ]);
  });

  it('state how old the sample is', () => {
    expect([
      toSampleLine(at(-20 * second), now),
      toSampleLine(at(minute), now),
      toSampleLine('soon', now),
      toSampleLine(undefined, now),
    ]).toEqual([
      `sampled ${local(at(-20 * second))} · 20s ago`,
      `sampled ${local(at(minute))} · no sample age`,
      'sampled soon · no sample age',
      'sampled — · no sample age',
    ]);
  });

  it('show the facts a read learned only when the read stood', () => {
    const evidence = { certificateID: 'c-1', role: 'apiserver', fingerprint: 'ab:cd' };
    const fingerprintOf = (isStanding: boolean, record?: typeof evidence | { role: string }) =>
      toCertificateFacts(record, isStanding)[2].value;
    expect([
      fingerprintOf(true, evidence),
      fingerprintOf(true, { role: 'apiserver' }),
      fingerprintOf(false, evidence),
      toCertificateFacts(undefined, true).map(({ value }) => value),
      toObservationFacts({}).map(({ value }) => value),
    ]).toEqual(['ab:cd', 'unknown', 'unknown', ['—', '—', '—', '—', '—'], ['—', '—']]);
  });

  it('show a renewal record as recorded', () => {
    expect([
      toRenewalFacts({ outcome: 'failed', operationID: 'op-1' }).map(({ label }) => label),
      toRenewalFacts({}),
    ]).toEqual([
      [
        'renewal outcome',
        'renewal attempted',
        'renewal completed',
        'renewal last success',
        'renewal operation',
      ],
      [
        { label: 'renewal outcome', value: '—' },
        { label: 'renewal attempted', value: '—' },
        { label: 'renewal completed', value: '—' },
        { label: 'renewal last success', value: '—' },
      ],
    ]);
  });

  it.each([
    [
      { code: 'certificate-unreadable', message: 'the store could not be read' },
      'read state corrupt — certificate-unreadable: the store could not be read',
    ],
    [
      { message: 'the store could not be read' },
      'read state corrupt — the store could not be read',
    ],
    [{ code: 'certificate-unreadable' }, 'read state corrupt — certificate-unreadable'],
    [undefined, 'read state corrupt'],
  ])('quote the reason %o beside a cause', (reason, line) => {
    expect(toCauseLine('read state corrupt', reason)).toBe(line);
  });
});

describe('the backup texts', () => {
  it.each([
    ['satisfactory', 'satisfactory'],
    ['partial', 'partial'],
    ['critical', 'critical'],
    ['disabled', 'disabled'],
    ['unknown', 'uncertain'],
    [undefined, 'uncertain'],
    ['unconfigured', 'unconfigured'],
    ['quarantined', 'quarantined'],
  ])('show the published outcome %s as %s', (outcome, word) => {
    expect(toVerdictWord(outcome)).toBe(word);
  });

  it('print an absent policy as a single line', () => {
    expect([toPolicyLines(undefined), toPolicyLines(null)]).toEqual([
      ['policy unknown'],
      ['policy unknown'],
    ]);
  });

  it('print the policy as published', () => {
    expect(
      toPolicyLines({
        state: 'configured',
        revision: 12,
        provenance: 'engine config',
        enabled: true,
        requiredLocations: ['local', 's3-east'],
        schedule: { expression: '0 2 * * *', timezone: 'UTC' },
        retention: { maxAgeSeconds: 604800, maxDistinctBackups: 14 },
        maxAgeSeconds: 86400,
      }),
    ).toEqual([
      'state configured',
      'revision 12',
      'provenance engine config',
      'enabled',
      'required locations local, s3-east',
      'schedule 0 2 * * * (UTC)',
      'retention keep 7d · 14 distinct backups',
      'copy age bound 24h',
    ]);
  });

  it('name what the policy does not state', () => {
    expect([
      toPolicyLines({}),
      toPolicyLines({
        enabled: false,
        schedule: { expression: '0 2 * * *' },
        retention: { maxDistinctBackups: 14 },
      }).filter((line) => /^(disabled|schedule|retention)/.test(line)),
      toPolicyLines({ retention: {} })[6],
    ]).toEqual([
      [
        'state unknown',
        'revision unknown',
        'provenance unknown',
        'enabled unknown',
        'required locations none',
        'schedule unknown',
        'retention unknown',
        'copy age bound unknown',
      ],
      ['disabled', 'schedule 0 2 * * *', 'retention 14 distinct backups'],
      'retention unknown',
    ]);
  });

  it.each([
    [
      { complete: true, returnedDistinctBackups: 3, totalDistinctBackups: 3 },
      'listing complete — 3 of 3 distinct backups served',
    ],
    [
      {
        complete: false,
        returnedDistinctBackups: 2,
        totalDistinctBackups: 5,
        reason: { message: 'page limit' },
      },
      'listing partial — 2 of 5 distinct backups served · page limit',
    ],
    [{ complete: false }, 'listing partial — distinct backup counts unknown'],
    [{}, 'listing partial — distinct backup counts unknown'],
  ])('state the listing %o', (listing, line) => {
    expect(toListingLine(listing)).toBe(line);
  });

  it('show each warning, labelled or not', () => {
    expect([toWarningText({ message: 'bucket nearly full' }), toWarningText({})]).toEqual([
      'bucket nearly full',
      'an unlabelled warning was reported',
    ]);
  });

  it('caption a backup with its creation time and size', () => {
    expect([
      toBackupCaption({
        backupID: 'b7',
        createdAt: '2026-01-05T02:00:00Z',
        sizeBytes: 1024,
        copies: [],
      }),
      toBackupCaption({ backupID: 'b7', copies: [] }),
    ]).toEqual(['created 2026-01-05T02:00:00Z · 1024 bytes', 'created unknown']);
  });

  it.each([
    [at(-4 * hour), 'age 4h old'],
    [undefined, 'never succeeded at its location'],
    ['soon', 'never succeeded at its location'],
    [at(10 * second), 'age 0s old'],
    [at(31 * second), 'age unknown — success timestamp exceeds allowed clock skew'],
  ])('measure a copy age from its last success %s', (lastSuccessAt, text) => {
    expect(
      toCopyAge({ copyID: 'c1', key: 'k', kind: 'remote', upload: { lastSuccessAt } }, now),
    ).toBe(text);
  });

  it.each([
    [
      'save',
      { outcome: 'success', attemptedAt: '2026-01-05T02:00:00Z' },
      'save success · attempted 2026-01-05T02:00:00Z',
    ],
    [
      'upload',
      {
        outcome: 'failed',
        attemptedAt: '2026-01-05T02:01:00Z',
        reason: { message: 'bucket unreachable' },
      },
      'upload failed · attempted 2026-01-05T02:01:00Z · bucket unreachable',
    ],
    ['upload', {}, 'upload unknown'],
    ['prune', { outcome: 'success' }, 'prune success'],
    ['prune', { outcome: 'failed', reason: { message: 'lock held' } }, 'prune failed · lock held'],
    ['prune', {}, 'prune unknown'],
  ])('show a %s attempt %o as "%s"', (label, attempt, line) => {
    expect(toAttemptLine(label, attempt)).toBe(line);
  });
});
