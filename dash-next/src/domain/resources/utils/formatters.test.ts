import { describe, expect, it } from 'vitest';

import {
  toAppliedNotice,
  toCountLine,
  toCreateCaption,
  toCreatedNotice,
  toCreatedText,
  toCreateTitle,
  toDeletedNotice,
  toDeleteHeading,
  toDeleteWarning,
  toEmptyMessage,
  toInvalidJsonNotice,
  toInvalidScaleNotice,
  toListMessage,
  toManifestName,
  toNewManifestName,
  toOpenPrefix,
  toQualifiedName,
  toRestartedNotice,
  toRestartHeading,
  toRestartWarning,
  toScaledNotice,
  toSearchName,
  toSearchPlaceholder,
  toStaleNotice,
  toSummary,
} from './formatters';

const pods = { kind: 'Pod', plural: 'Pods', isNamespaced: true };
const storageClasses = { kind: 'StorageClass', plural: 'StorageClasses', isNamespaced: false };
const deployments = { kind: 'Deployment', plural: 'Deployments', isNamespaced: true };

const api = { metadata: { name: 'api', namespace: 'payments' } };
const payments = { metadata: { name: 'payments' } };

describe('toCountLine', () => {
  it.each([
    [3, 3, '3 Pods'],
    [1, 1, '1 Pod'],
    [0, 0, '0 Pods'],
    [4, 12, '4 of 12 Pods'],
    [1, 12, '1 of 12 Pod'],
  ])('counts %s shown of %s loaded as "%s"', (shown, total, text) => {
    expect(toCountLine(shown, total, pods, undefined)).toBe(text);
  });

  it('names the selected namespace for a namespaced kind', () => {
    expect(toCountLine(0, 12, pods, 'team-b')).toBe('0 of 12 Pods · team-b');
  });

  it('never names a namespace for a cluster-scoped kind', () => {
    expect(toCountLine(2, 2, storageClasses, 'kube-system')).toBe('2 StorageClasss');
  });
});

describe('toSummary', () => {
  it('says it is loading until the first answer arrives', () => {
    expect(toSummary('loading', 0, 0, pods, undefined)).toBe('Loading…');
  });

  it('counts the rows once the list answered', () => {
    expect(toSummary('ready', 4, 12, pods, undefined)).toBe('4 of 12 Pods');
  });

  it('counts zero for a first load that failed', () => {
    expect([
      toSummary('failed', 0, 0, pods, undefined),
      toSummary('forbidden', 0, 0, pods, undefined),
    ]).toEqual(['0 Pods', '0 Pods']);
  });
});

describe('toListMessage', () => {
  const unableToLoad =
    'Unable to load — the apiserver returned an error (or the RBAC lacks access).';

  it('shows no message while loading or while rows are visible', () => {
    expect([
      toListMessage('loading', false, 0, pods),
      toListMessage('failed', true, 3, pods),
    ]).toEqual([undefined, undefined]);
  });

  it('says nothing was found when the list answered and no row is visible', () => {
    expect(toListMessage('ready', true, 0, pods)).toBe('No pods found.');
  });

  it('shows an error in place of the rows for a first load that failed', () => {
    expect(toListMessage('failed', false, 0, pods)).toBe(unableToLoad);
  });

  it('says a forbidden read is not permitted, in words distinct from a failed load', () => {
    expect(toListMessage('forbidden', false, 0, pods)).toBe('You are not permitted to list pods.');
  });

  it('keeps "nothing found" for a loaded list whose refresh failed while no row is visible', () => {
    expect(toListMessage('failed', true, 0, pods)).toBe('No pods found.');
  });
});

describe('toStaleNotice', () => {
  it('marks loaded rows as stale while the latest refresh failed', () => {
    expect([toStaleNotice('failed', true), toStaleNotice('forbidden', true)]).toEqual([
      'The latest refresh failed. Showing the last data that loaded.',
      'The latest refresh failed. Showing the last data that loaded.',
    ]);
  });

  it('is absent after a successful refresh and before anything loaded', () => {
    expect([
      toStaleNotice('ready', true),
      toStaleNotice('failed', false),
      toStaleNotice('loading', false),
    ]).toEqual([undefined, undefined, undefined]);
  });
});

describe('accessible names', () => {
  it('name the search box and the manifest editors', () => {
    expect([toSearchName(pods), toManifestName(deployments, api), toNewManifestName(pods)]).toEqual(
      ['Search Pods', 'Deployment api manifest', 'New Pod manifest'],
    );
  });
});

describe('labels of the list', () => {
  it('names the kind in the empty message, the search box and the row label', () => {
    expect([
      toEmptyMessage(storageClasses),
      toSearchPlaceholder(storageClasses),
      toOpenPrefix(pods),
    ]).toEqual(['No storageclasses found.', 'Search storageclasses…', 'Open Pod']);
  });
});

describe('toQualifiedName', () => {
  it('prefixes the namespace of a namespaced object only', () => {
    expect([toQualifiedName(api), toQualifiedName(payments)]).toEqual(['payments/api', 'payments']);
  });
});

describe('toCreatedText', () => {
  it('reads the age followed by ago, or a dash without a creation time', () => {
    expect([toCreatedText('5m'), toCreatedText('—')]).toEqual(['5m ago', '—']);
  });
});

describe('create panel wording', () => {
  it('titles the panel with the kind', () => {
    expect(toCreateTitle(deployments)).toBe('Create Deployment');
  });

  it('says where the manifest will be posted', () => {
    expect([toCreateCaption(deployments), toCreateCaption(storageClasses)]).toEqual([
      'Manifest (JSON) — POST to the namespace',
      'Manifest (JSON) — POST to the cluster',
    ]);
  });
});

describe('confirmation wording', () => {
  it('names the kind and the object to delete', () => {
    expect([toDeleteHeading(deployments), toDeleteWarning(api), toDeleteWarning(payments)]).toEqual(
      [
        'Delete Deployment?',
        'payments/api will be permanently deleted.',
        'payments will be permanently deleted.',
      ],
    );
  });

  it('names the workload to restart', () => {
    expect([toRestartHeading(deployments), toRestartWarning(api)]).toEqual([
      'Restart Deployment?',
      'Triggers a rolling restart of api — all its pods are recreated.',
    ]);
  });
});

describe('notices', () => {
  it('announce an accepted write', () => {
    expect([
      toAppliedNotice(deployments, api),
      toCreatedNotice(deployments, api),
      toDeletedNotice(deployments, api),
      toRestartedNotice(api),
    ]).toEqual([
      'Applied changes to Deployment api',
      'Created Deployment api',
      'Deleted Deployment api',
      'Rolling restart of api triggered',
    ]);
  });

  it.each([
    [0, 'Scaled api → 0 replicas'],
    [1, 'Scaled api → 1 replica'],
    [5, 'Scaled api → 5 replicas'],
  ])('announce a scale to %s', (replicas, notice) => {
    expect(toScaledNotice(api, replicas)).toBe(notice);
  });

  it("report text that is not JSON with the parser's message", () => {
    expect(toInvalidJsonNotice('Unexpected end of JSON input')).toBe(
      'Invalid JSON: Unexpected end of JSON input',
    );
  });
});

describe('toInvalidScaleNotice', () => {
  it('quotes the value that is not a replica count', () => {
    expect([toInvalidScaleNotice(1.9), toInvalidScaleNotice(null)]).toEqual([
      'Error: "1.9" is not a replica count',
      'Error: "" is not a replica count',
    ]);
  });
});
