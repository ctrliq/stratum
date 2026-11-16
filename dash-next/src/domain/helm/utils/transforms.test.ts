import { describe, expect, it } from 'vitest';

import { toHelmChartConfigRow, toHelmChartRow, toHelmRelease, toHelmReleases } from './transforms';

const now = Date.parse('2026-11-13T12:00:00Z');

const toSecret = (name: string, version: string, status: string, hoursAgo: number) => ({
  metadata: {
    name: `sh.helm.release.v1.${name}.v${version}`,
    namespace: 'shop',
    creationTimestamp: new Date(now - hoursAgo * 3_600_000).toISOString(),
    labels: { name, owner: 'helm', status, version },
  },
});

describe('toHelmReleases', () => {
  it('shows one release per namespace and name, at its highest revision', () => {
    expect(
      toHelmReleases(
        [
          toSecret('cart', '1', 'superseded', 30),
          toSecret('cart', '2', 'superseded', 20),
          toSecret('cart', '3', 'deployed', 5),
          toSecret('search', '1', 'failed', 8),
        ],
        now,
      ),
    ).toEqual([
      {
        name: 'cart',
        namespace: 'shop',
        chart: '—',
        appVersion: '—',
        status: 'deployed',
        revision: 3,
        updated: '5h',
      },
      {
        name: 'search',
        namespace: 'shop',
        chart: '—',
        appVersion: '—',
        status: 'failed',
        revision: 1,
        updated: '8h',
      },
    ]);
  });
});

describe('toHelmRelease', () => {
  it('shows a dash for chart, app version and a missing status', () => {
    const release = toHelmRelease({ metadata: { name: 'cart', namespace: 'shop' } }, now);
    expect(release.chart).toBe('—');
    expect(release.appVersion).toBe('—');
    expect(release.status).toBe('—');
  });

  it('takes the Secret’s own name without a name label', () => {
    expect(toHelmRelease({ metadata: { name: 'cart-secret' } }, now).name).toBe('cart-secret');
  });

  it('counts a revision label that is not a number as revision 0', () => {
    expect(toHelmRelease({ metadata: { labels: { version: 'latest' } } }, now).revision).toBe(0);
  });

  it('reads "updated" as the age of the Secret', () => {
    expect(toHelmRelease(toSecret('cart', '3', 'deployed', 5), now).updated).toBe('5h');
  });
});

describe('chart rows', () => {
  it('reads chart, version, target namespace and phase', () => {
    expect(
      toHelmChartRow({
        spec: { chart: 'podinfo', version: '6.7.1', targetNamespace: 'shop' },
        status: { phase: 'Deployed' },
      }),
    ).toEqual({ chart: 'podinfo', version: '6.7.1', targetNamespace: 'shop', phase: 'Deployed' });
  });

  it('shows a dash for unset fields and Pending for no phase', () => {
    expect(toHelmChartRow({})).toEqual({
      chart: '—',
      version: '—',
      targetNamespace: '—',
      phase: 'Pending',
    });
  });

  it('sizes the values of a chart config', () => {
    expect(toHelmChartConfigRow({ spec: { valuesContent: 'a: 1\n\nb: 2' } })).toEqual({
      values: '2 lines',
    });
    expect(toHelmChartConfigRow({})).toEqual({ values: '0 lines' });
  });
});
