import { describe, expect, it } from 'vitest';

import { ago, appWith, chartWith, now, paramWith } from '../testing/fixtures';
import {
  toCountText,
  toDroppedNotice,
  toEmptyMessage,
  toFooterCaption,
  toInstalledAge,
  toInstallFailureNotice,
  toInstallingNotice,
  toParamLabel,
  toPhaseText,
  toRequiredLine,
  toRereadDelay,
  toSecretHint,
  toStateText,
  toStateTone,
  toStatusText,
  toStuckWarning,
  toSubtitle,
  toUninstallingCaption,
  toUninstallingNotice,
  toUntargetedNotice,
} from './formatters';

const loki = appWith();
const created = chartWith({ metadata: { creationTimestamp: ago(300) } });
const leaving = chartWith({ metadata: { deletionTimestamp: ago(30) } });
const appsOf = (count: number) => Array.from({ length: count }, () => loki);

describe('toSubtitle', () => {
  it.each([
    [1, 'configmap', 0, '1 application · source: ConfigMap · 0 installed'],
    [12, 'configmap', 3, '12 applications · source: ConfigMap · 3 installed'],
    [12, 'remote', 3, '12 applications · source: remote URL · 3 installed'],
    [12, 'configmap', undefined, '12 applications · source: ConfigMap · install state unknown'],
    [0, 'configmap', 0, '0 applications · source: ConfigMap · 0 installed'],
  ] as const)('%s apps from %s with %s installed', (count, source, installed, subtitle) => {
    expect(toSubtitle({ apps: appsOf(count), source }, installed)).toBe(subtitle);
  });
});

describe('toCountText and toEmptyMessage', () => {
  it('show a dash for an unknown count', () => {
    expect([toCountText(4), toCountText(undefined)]).toEqual(['4', '—']);
  });

  it.each([
    ['zzz', 'No apps match "zzz".'],
    ['  zzz  ', 'No apps match "zzz".'],
    ['', 'No apps match this filter.'],
  ])('explain an empty result for the search %o', (search, message) => {
    expect(toEmptyMessage(search)).toBe(message);
  });
});

describe('toStatusText and toStateText', () => {
  it('state the status shown in the table and the essentials strip', () => {
    expect([
      toStatusText('deployed', created, now),
      toStatusText('deployed', chartWith(), now),
      toStatusText('failed', created, now),
      toStatusText('installing', created, now),
      toStatusText('removing', leaving, now),
      toStatusText('stuck', chartWith({ metadata: { deletionTimestamp: ago(600) } }), now),
    ]).toEqual([
      'Deployed 5m ago',
      'Deployed',
      'Failed',
      'Installing',
      'Removing 30s',
      'Still removing 10m',
    ]);
  });

  it('leave the age out of the header state', () => {
    expect([toStateText('deployed', created, now), toInstalledAge(created, now)]).toEqual([
      'Deployed',
      '5m ago',
    ]);
  });
});

describe('toPhaseText and toStateTone', () => {
  it.each([
    ['deployed', chartWith({ status: { phase: 'Deployed' } }), 'Deployed', 'success'],
    ['failed', chartWith({ status: { phase: 'Failed' } }), 'Failed', 'error'],
    ['installing', chartWith({ status: { phase: 'Upgrading' } }), 'Upgrading', 'neutral'],
    ['installing', chartWith(), 'Pending', 'neutral'],
    ['removing', leaving, 'Removing (30s)', 'neutral'],
    [
      'stuck',
      chartWith({ metadata: { deletionTimestamp: ago(600) } }),
      'Removing (stuck 10m)',
      'error',
    ],
  ] as const)('a %s release reads %s', (state, chart, text, tone) => {
    expect([toPhaseText(state, chart, now), toStateTone(state)]).toEqual([text, tone]);
  });
});

describe('removal captions', () => {
  it('say how long the removal has run', () => {
    const stuck = chartWith({ metadata: { deletionTimestamp: ago(180) } });
    expect([toUninstallingCaption(leaving, now), toStuckWarning(stuck, now)]).toEqual([
      'Uninstalling… 30s',
      'Still removing after 3m — check the stratum-operator logs. The HelmChart keeps its finalizer until helm uninstall succeeds, so this will not clear on its own.',
    ]);
  });
});

describe('toFooterCaption', () => {
  it('states how an install would run, or that it cannot', () => {
    const timed = appWith({
      install: { targetNamespace: '', createNamespace: true, timeout: '10m' },
    });
    expect([
      toFooterCaption(timed, true),
      toFooterCaption(loki, true),
      toFooterCaption(loki, false),
    ]).toEqual([
      'Helm install · timeout 10m',
      'Helm install · timeout 5m',
      'Install state unknown — cannot install safely',
    ]);
  });
});

describe('form captions', () => {
  it('name unsatisfied parameters, the credentials and the required mark', () => {
    const admin = paramWith({ key: 'a', label: 'Admin password' });
    const database = paramWith({ key: 'b', label: 'DB password', isRequired: true });
    expect([
      toRequiredLine([admin, database]),
      toSecretHint([admin, database]),
      toParamLabel(admin),
      toParamLabel(database),
    ]).toEqual([
      'Required: Admin password, DB password',
      "Name of a Secret in kube-system holding Admin password, DB password. Leave empty to keep the chart's defaults.",
      'Admin password',
      'DB password *',
    ]);
  });
});

describe('install notices', () => {
  it('announce an accepted install', () => {
    expect(toInstallingNotice(loki, 'logging')).toBe('Installing Loki — loki 6.1.0 → ns logging');
  });

  it.each([
    [{ secret: 'my-creds', keys: [] }, 'spec.valuesSecrets (Secret my-creds)'],
    [{ secret: undefined, keys: ['a', 'b'] }, 'spec.setString (a, b)'],
    [{ secret: undefined, keys: ['b'] }, 'spec.setString (b)'],
    [
      { secret: 'my-creds', keys: ['a'] },
      'spec.valuesSecrets (Secret my-creds) or spec.setString (a)',
    ],
  ])('announce values the cluster discarded: %o', (dropped, named) => {
    expect(toDroppedNotice(loki, dropped)).toBe(
      `Installing Loki, but this cluster's Stratum operator does not support ${named}: ignored, so the chart's defaults apply; upgrade the cluster to engine v1.37.1-stratum.0 or later`,
    );
  });

  it.each([
    [
      { status: 409, payload: { message: 'exists' } },
      'A HelmChart named "loki" already exists in kube-system',
    ],
    [
      { status: 403, payload: { message: 'helmcharts is forbidden' } },
      'Error: helmcharts is forbidden',
    ],
    [{ status: 500, payload: 'upstream broke' }, 'Error: upstream broke'],
    [{ status: 502, payload: '' }, 'Error: HTTP 502'],
    [{ status: 'unknown', payload: 'Failed to fetch' }, 'Error: Failed to fetch'],
    [
      { status: 'unknown', payload: 'The server answered with a redirect', reason: 'redirect' },
      'Error: the write was redirected, and the dashboard does not follow redirects on writes',
    ],
  ])('report a failed install %o', (error, notice) => {
    expect(toInstallFailureNotice(error, 'loki')).toBe(notice);
  });
});

describe('uninstall notices', () => {
  it('name the namespace the release is removed from', () => {
    expect([
      toUninstallingNotice(loki, { targetNamespace: 'logging' }),
      toUninstallingNotice(loki, { targetNamespace: '' }),
      toUntargetedNotice(loki),
    ]).toEqual([
      'Uninstalling Loki — the operator is removing the release from logging',
      'Uninstalling Loki — the operator is removing the release from —',
      'Error: cannot uninstall Loki — its HelmChart reports no namespace',
    ]);
  });
});

describe('toRereadDelay', () => {
  it.each([
    [15_000, 0, 30_000],
    [15_000, 1, 60_000],
    [15_000, 2, 120_000],
    [15_000, 6, 120_000],
    [3_000, 0, 6_000],
    [3_000, 3, 48_000],
  ])(
    'from a %s ms cadence after %s earlier failures centres on %s ms',
    (cadence, failures, centre) => {
      expect([
        toRereadDelay(cadence, failures, 0),
        toRereadDelay(cadence, failures, 0.5),
        toRereadDelay(cadence, failures, 1),
      ]).toEqual([Math.round(centre * 0.85), centre, Math.round(centre * 1.15)]);
    },
  );
});
