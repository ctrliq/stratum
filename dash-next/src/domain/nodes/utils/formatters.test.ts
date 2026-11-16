import { describe, expect, it } from 'vitest';

import { emptyUsage } from '~/domain/overview/constants';
import { absentText } from '~/shared/utils/formatters';

import { finding, load, observation, rowsOf } from '../testing/fixtures';
import {
  toAcceleratorsText,
  toCollectingText,
  toCordonLabel,
  toCordonNotice,
  toCpuCaption,
  toCpuDetail,
  toDetailSubtitle,
  toDriftMarker,
  toDriftSummary,
  toDriftTitle,
  toIdentityText,
  toIdentityTitle,
  toLabelText,
  toMemoryCaption,
  toMemoryDetail,
  toNodeCount,
  toNormalVersion,
  toPodsText,
  toPodTotal,
  toRolloutText,
  toRolloutTitle,
  toRollupLine,
  toServingDetail,
  toServingText,
  toSummaryLine,
  toTaintText,
  toUnavailableNotice,
  toUptime,
  toVerdictText,
} from './formatters';
import { toVersionView } from './transforms';

const totals = {
  ...emptyUsage.totals,
  nodes: 4,
  cpuUsedCores: 3.5,
  cpuTotalCores: 16,
  memoryUsedGibibytes: 12,
  memoryTotalGibibytes: 64,
};

describe('toTaintText', () => {
  it('reads a taint with a value as key=value:effect', () => {
    expect(toTaintText({ key: 'dedicated', value: 'gpu', effect: 'NoSchedule' })).toBe(
      'dedicated=gpu:NoSchedule',
    );
  });

  it('reads a taint with no value as key:effect', () => {
    expect(toTaintText({ key: 'node.kubernetes.io/unreachable', effect: 'NoExecute' })).toBe(
      'node.kubernetes.io/unreachable:NoExecute',
    );
  });
});

describe('toLabelText', () => {
  it('shows a label with a value as key=value and one without as the key', () => {
    expect(toLabelText(['tier', 'edge'])).toBe('tier=edge');
    expect(toLabelText(['spot', ''])).toBe('spot');
  });
});

describe('toSummaryLine', () => {
  it('counts ready and cordoned nodes, a cordoned ready node being ready', () => {
    expect(
      toSummaryLine([
        { readiness: 'Ready', isCordoned: true },
        { readiness: 'Ready', isCordoned: false },
        { readiness: 'Unknown', isCordoned: false },
      ]),
    ).toBe('2 of 3 ready · 1 cordoned');
  });

  it('names the drift count', () => {
    expect(toDriftSummary(2)).toBe('2 version drift');
  });
});

describe('row texts', () => {
  it('shows pods over capacity', () => {
    expect(toPodsText({ podCount: 10, podCapacity: 110 })).toBe('10/110');
  });

  it('explains a drift', () => {
    expect(toDriftTitle({ stratumVersion: 'v1.36.5', kubeletVersion: 'v1.36.4' })).toBe(
      'stamped k8s-version v1.36.5 differs from kubelet v1.36.4',
    );
  });
});

describe('detail texts', () => {
  it('reads the subtitle, uptime and capacity rows', () => {
    expect(toDetailSubtitle({ role: 'control-plane', zone: 'eu-1a' })).toBe(
      'control-plane · eu-1a',
    );
    expect(toUptime('3d')).toBe('up 3d');
    expect(toUptime(absentText)).toBe(absentText);
    expect(toCpuDetail(42, 8)).toBe('42% · 8 vCPU');
    expect(toMemoryDetail(61)).toBe('61% · of allocatable');
    expect(toPodTotal(3)).toBe('3 total');
  });

  it('lists every accelerator, or none', () => {
    expect(
      toAcceleratorsText([
        { name: 'nvidia.com/gpu', count: '4' },
        { name: 'amd.com/gpu', count: '1' },
      ]),
    ).toBe('nvidia.com/gpu ×4, amd.com/gpu ×1');
    expect(toAcceleratorsText([])).toBe('none');
  });
});

describe('cordon texts', () => {
  it('labels the action and reports its outcome', () => {
    expect(toCordonLabel(false)).toBe('Cordon');
    expect(toCordonLabel(true)).toBe('Uncordon');
    expect(toCordonNotice('worker-1', true)).toBe('worker-1 cordoned — no new pods will schedule');
    expect(toCordonNotice('worker-1', false)).toBe('worker-1 uncordoned — schedulable again');
  });
});

describe('usage captions', () => {
  it('reads used over total and the node count', () => {
    expect(toCpuCaption(totals)).toBe('3.50 / 16 cores · 4 nodes');
    expect(toMemoryCaption(totals)).toBe('12.0 / 64.0 GiB · 4 nodes');
  });

  it('uses the singular for one node', () => {
    expect(toNodeCount(1)).toBe('1 node');
    expect(toCpuCaption({ ...totals, nodes: 1 })).toMatch(/· 1 node$/);
  });

  it('says how many samples were collected', () => {
    expect(toCollectingText(2)).toBe('collecting · 2 of 24 samples');
  });
});

describe('toUnavailableNotice', () => {
  it.each([
    ['detail-expired', 'the report detail expired'],
    ['generation-changed', 'the report generation moved on before the view could read it'],
    ['summary-unavailable', 'the engine state summary is unavailable'],
    ['anything else', 'the engine state could not be read'],
    [undefined, 'the engine state could not be read'],
  ])('explains the reason %s', (reason, text) => {
    expect(toUnavailableNotice(reason)).toBe(
      `Version comparison unavailable — ${text}. No values are shown rather than guessed.`,
    );
  });
});

describe('toNormalVersion and toDriftMarker', () => {
  it('reduce a version to major.minor.patch', () => {
    expect([
      toNormalVersion('v1.36.4+k3s1'),
      toNormalVersion('1.36'),
      toNormalVersion(undefined),
    ]).toEqual(['1.36.4', '1.36.0', '0.0.0']);
    expect(toDriftMarker('v1.36.4-stratum.2')).toBe('drift → 1.36.4');
  });
});

describe('version cells', () => {
  const reason = { code: 'source-timeout', message: 'member did not answer' };
  const unknown = { raw: undefined, availability: undefined, digest: undefined, reason };

  it('shows the reported value verbatim or an explicit Unknown with its reason', () => {
    expect(toIdentityText({ ...unknown, raw: 'v1.36.4' })).toBe('v1.36.4');
    expect(toIdentityTitle({ ...unknown, raw: 'v1.36.4' }, 'installed')).toBeUndefined();
    expect(toIdentityText(unknown)).toBe('Unknown');
    expect(toIdentityTitle(unknown, 'installed')).toBe('source-timeout: member did not answer');
    expect(toIdentityTitle(undefined, 'installed')).toBe('no installed value was reported');
  });

  it('tells running, installed-only and unknown serving apart', () => {
    const running = { ...unknown, raw: 'v1.36.4' };
    expect(toServingText({ serving: running, servingLabel: 'Running' })).toBe('v1.36.4');
    expect(toServingText({ serving: undefined, servingLabel: 'Installed' })).toBe('Installed');
    expect(toServingText({ serving: unknown, servingLabel: undefined })).toBe('Unknown');
    expect(toServingDetail({ serving: undefined, servingLabel: 'Installed' })).toBe(
      'Installed (no serving process)',
    );
  });

  it("shows the hub's own verdict, or unknown", () => {
    expect(toVerdictText({ verdict: 'drift' })).toBe('drift');
    expect(toVerdictText({ verdict: undefined })).toBe('unknown');
  });

  it('names the rollout and its target release', () => {
    const [row] = rowsOf(
      load(
        [finding({ code: 'outcome-info' })],
        [
          observation({
            intent: {
              phase: 'running',
              operationID: 'op-1',
              action: 'release.upgrade',
              deadline: '2026-01-05T12:00:00Z',
              targetRelease: { releaseID: 'r-42' },
            },
          }),
        ],
      ),
    );
    expect(row && toRolloutText(row)).toBe('Upgrading → r-42');
    expect(row && toRolloutTitle(row)).toBe(
      'running operation op-1 (release.upgrade) intends r-42, due 2026-01-05T12:00:00Z',
    );
    expect(
      toRolloutText({ rolloutLabel: 'Upgrading', intentLabel: 'running', intent: undefined }),
    ).toBe('Upgrading → unknown release');
    expect(toRolloutTitle({ intentLabel: 'running', intent: undefined })).toContain(
      'intends an unstated release',
    );
  });

  it("carries the hub's roll-up in the subtitle, unknown where it is missing", () => {
    const view = toVersionView(load([]));
    const line = view.isReachable ? toRollupLine(view) : '';
    expect(line).toContain('observed 2026-01-05T10:00:00Z');
    expect(line).toContain('hub rollup warning · reporting fresh');
    expect(line).toContain('coverage 5/6, 1 unknown');
    expect(
      toRollupLine({ observedAt: undefined, severity: '', freshness: '', coverage: undefined }),
    ).toMatch(/hub rollup unknown · reporting unknown$/);
  });
});
