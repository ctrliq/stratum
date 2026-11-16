import { describe, expect, it } from 'vitest';

import {
  toCpuText,
  toDegradedFlag,
  toEmptyWorkloadsMessage,
  toKindLabel,
  toMemoryText,
  toObservedText,
  toPodKey,
  toReadyCount,
  toSuspendText,
  toUnreadableNotice,
  toWorkloadsSummary,
} from './formatters';

describe('toReadyCount', () => {
  it.each([
    [2, 2, '2/2'],
    [1, 3, '1/3'],
    [0, 0, '0/0'],
  ])('reads %s ready of %s as %s', (ready, total, shown) => {
    expect(toReadyCount(ready, total)).toBe(shown);
  });
});

describe('toPodKey', () => {
  it('joins the namespace and the name', () => {
    expect(toPodKey({ namespace: 'shop', name: 'web-1' })).toBe('shop/web-1');
  });
});

describe('toCpuText', () => {
  it.each([
    [0, '—'],
    [250, '250m'],
    [999.6, '1000m'],
    [1000, '1.00 cores'],
    [2500, '2.50 cores'],
  ])('reads %s millicores as %s', (millicores, text) => {
    expect(toCpuText(millicores)).toBe(text);
  });
});

describe('toMemoryText', () => {
  it.each([
    [0, '—'],
    [512, '512 Mi'],
    [1024, '1.0 GiB'],
    [1536, '1.5 GiB'],
  ])('reads %s MiB as %s', (mebibytes, text) => {
    expect(toMemoryText(mebibytes)).toBe(text);
  });
});

describe('toWorkloadsSummary', () => {
  it.each([
    [7, 7, undefined, '7 workloads'],
    [2, 7, undefined, '2 of 7 workloads'],
    [2, 7, 'payments', '2 of 7 workloads · payments'],
    [1, 1, undefined, '1 workloads'],
  ])('reads %s shown of %s in %s as %s', (shown, total, namespace, text) => {
    expect(toWorkloadsSummary(shown, total, namespace)).toBe(text);
  });
});

describe('toDegradedFlag', () => {
  it('counts the degraded workloads', () => {
    expect(toDegradedFlag(2)).toBe(' · 2 degraded');
  });
});

describe('toEmptyWorkloadsMessage', () => {
  it('says what was looked for', () => {
    expect(toEmptyWorkloadsMessage(undefined)).toBe('No workloads found.');
    expect(toEmptyWorkloadsMessage('Deployment')).toBe('No workloads of kind Deployment found.');
  });
});

describe('toUnreadableNotice', () => {
  it('names the kind that was refused or could not be read', () => {
    expect(toUnreadableNotice({ plural: 'CronJobs', state: 'forbidden' })).toBe(
      'You are not permitted to list cronjobs.',
    );
    expect(toUnreadableNotice({ plural: 'Jobs', state: 'failed' })).toBe('Jobs could not be read.');
  });
});

describe('toKindLabel', () => {
  it('carries the count, and no number for a kind without workloads', () => {
    expect(toKindLabel('All', 7)).toBe('All 7');
    expect(toKindLabel('Job', 0)).toBe('Job');
    expect(toKindLabel('Job', undefined)).toBe('Job');
  });
});

describe('toObservedText', () => {
  it('says when the figure was observed', () => {
    expect(toObservedText('2026-11-13T10:00:00Z')).toBe('Observed 2026-11-13T10:00:00Z');
  });
});

describe('toSuspendText', () => {
  it.each([
    [true, 'true'],
    [false, 'false'],
    [undefined, 'false'],
  ])('reads %s as %s', (isSuspended, text) => {
    expect(toSuspendText(isSuspended)).toBe(text);
  });
});
