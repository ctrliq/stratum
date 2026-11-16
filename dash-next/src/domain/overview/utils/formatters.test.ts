import { describe, expect, it } from 'vitest';

import {
  toCapacityCaption,
  toHeaderLine,
  toKubernetesText,
  toNodeLinkTitle,
  toRatio,
  toRunningSentence,
  toRunningText,
  toShortRole,
  toVersionText,
} from './formatters';

describe('toHeaderLine', () => {
  it('reads context, provider, region and version', () => {
    expect(toHeaderLine('Production EU · aws', 'eu-west-1', '1.37.1')).toBe(
      'Production EU · aws · eu-west-1 · Kubernetes v1.37.1',
    );
    expect(toHeaderLine('local · —', 'eu-west-1a', '1.37.1')).toBe(
      'local · — · eu-west-1a · Kubernetes v1.37.1',
    );
  });

  it('ends after the region when the version is unknown', () => {
    expect(toHeaderLine('local · —', '—', '')).toBe('local · — · —');
  });

  it('shows the version with exactly one leading v', () => {
    expect(toVersionText('v1.37.1')).toBe('v1.37.1');
    expect(toVersionText('1.37.1')).toBe('v1.37.1');
    expect(toKubernetesText('')).toBe('—');
  });
});

describe('overview texts', () => {
  it('formats counts, roles and link titles', () => {
    expect(toRatio(2, 3)).toBe('2 / 3');
    expect(toCapacityCaption(20)).toBe('20% of capacity');
    expect(toShortRole('control-plane')).toBe('control');
    expect(toShortRole('worker')).toBe('worker');
    expect(toNodeLinkTitle('cp-1')).toBe('Open cp-1 on the Nodes page');
    expect(toRunningText({ running: 3, total: 3 })).toBe('3/3');
    expect(toRunningSentence({ running: 9, total: 9 })).toBe('9/9 running');
  });
});
