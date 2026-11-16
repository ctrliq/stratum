import { describe, expect, it } from 'vitest';

import { noFilters } from '../constants';
import { appWith, chartWith } from '../testing/fixtures';
import { essentialsAmong, matchingFilters } from './filters';

const core = appWith({ id: 'cni', name: 'CNI', category: 'Core' });
const grafana = appWith({ id: 'grafana', name: 'Grafana', category: 'Monitoring' });
const prometheus = appWith({ id: 'prometheus', name: 'Prometheus', category: 'Monitoring' });
const apps = [core, grafana, prometheus];
const releases = new Map([
  ['grafana', chartWith()],
  ['cni', chartWith()],
]);
const idsOf = (matching: { id: string }[]) => matching.map(({ id }) => id);

describe('matchingFilters', () => {
  it('lists everything without filters', () => {
    expect(idsOf(matchingFilters(apps, noFilters, new Map()))).toEqual([
      'cni',
      'grafana',
      'prometheus',
    ]);
  });

  it('narrows by category, by release and by search, and combines them', () => {
    expect([
      idsOf(matchingFilters(apps, { ...noFilters, category: 'Monitoring' }, releases)),
      idsOf(matchingFilters(apps, { ...noFilters, isInstalledOnly: true }, releases)),
      idsOf(matchingFilters(apps, { ...noFilters, search: 'graf' }, releases)),
      idsOf(
        matchingFilters(
          apps,
          { search: 'a', category: 'Monitoring', isInstalledOnly: true },
          releases,
        ),
      ),
    ]).toEqual([['grafana', 'prometheus'], ['cni', 'grafana'], ['grafana'], ['grafana']]);
  });

  it('lists nothing under the Installed filter before any release was read', () => {
    expect(matchingFilters(apps, { ...noFilters, isInstalledOnly: true }, new Map())).toEqual([]);
  });
});

describe('essentialsAmong', () => {
  it('features the Core applications only while the list is not narrowed', () => {
    expect([
      idsOf(essentialsAmong(apps, noFilters)),
      essentialsAmong(apps, { ...noFilters, search: 'x' }),
      essentialsAmong(apps, { ...noFilters, category: 'Monitoring' }),
      essentialsAmong(apps, { ...noFilters, isInstalledOnly: true }),
      essentialsAmong([grafana], noFilters),
    ]).toEqual([['cni'], [], [], [], []]);
  });
});
