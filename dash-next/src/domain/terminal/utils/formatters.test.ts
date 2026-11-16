import { describe, expect, it } from 'vitest';

import {
  toConnectingLine,
  toCreateRefusal,
  toDrawerScope,
  toErrorLine,
  toFailedShellLabel,
  toHelperLabel,
  toHostPlaceholder,
  toHostShellFailure,
  toLineCounter,
  toNodeTabId,
  toPodEndedMessage,
  toPodOption,
  toPodReference,
  toPodTabId,
  toSessionLabel,
} from './formatters';

const pod = { namespace: 'shop', name: 'web-1' };

describe('toSessionLabel', () => {
  it('names the pod, the container and the command', () => {
    expect(toSessionLabel({ namespace: 'shop', pod: 'web-1', container: 'app' })).toBe(
      'shop/web-1 · app · /bin/sh',
    );
  });

  it('shows no container when none is named', () => {
    expect(toSessionLabel({ namespace: 'shop', pod: 'web-1', command: 'bash -l' })).toBe(
      'shop/web-1 · bash -l',
    );
  });
});

describe('terminal lines', () => {
  it('announce the connection in a dim colour', () => {
    expect(toConnectingLine({ namespace: 'shop', pod: 'web-1' })).toBe(
      '\u001b[90mconnecting to shop/web-1 …\u001b[0m\r\n',
    );
  });
});

describe('log texts', () => {
  it('prefix errors and count lines', () => {
    expect([toErrorLine('HTTP 403'), toLineCounter(1), toLineCounter(250)]).toEqual([
      '[error] HTTP 403',
      '1 lines',
      '250 lines',
    ]);
  });
});

describe('identities', () => {
  it('tell tabs and picker entries apart', () => {
    expect([
      toPodTabId(pod),
      toNodeTabId('worker-1'),
      toPodOption(pod),
      toPodReference(pod),
    ]).toEqual(['pod:shop/web-1', 'node:worker-1', 'shop / web-1', 'shop/web-1']);
  });
});

describe('host shell texts', () => {
  it('cut the refusal detail at 180 characters', () => {
    expect(toCreateRefusal({ status: 403, payload: 'x'.repeat(200) })).toBe(
      `create debug job → HTTP 403 ${'x'.repeat(180)}`,
    );
  });

  it('name the phase, the node and the pod', () => {
    expect([
      toPodEndedMessage('Failed'),
      toHostShellFailure('debug pod Failed'),
      toHelperLabel('worker-1'),
      toHelperLabel('worker-1', 'shell-x'),
      toFailedShellLabel('worker-1'),
      toHostPlaceholder('worker-1'),
    ]).toEqual([
      'debug pod Failed',
      'Host shell failed: debug pod Failed',
      'node worker-1',
      'node worker-1 · shell-x',
      'node-shell · worker-1',
      'nsenter → host namespaces on worker-1…',
    ]);
  });
});

describe('toDrawerScope', () => {
  it('changes with the cluster, the user and the sign-in', () => {
    const scopes = [
      toDrawerScope('edge-7', 'ada', 't1'),
      toDrawerScope('edge-8', 'ada', 't1'),
      toDrawerScope('edge-7', 'bob', 't1'),
      toDrawerScope('edge-7', 'ada', 't2'),
    ];
    expect(new Set(scopes).size).toBe(4);
  });

  it('is the same for the same cluster under the same sign-in', () => {
    expect(toDrawerScope(undefined, '', undefined)).toBe(toDrawerScope(undefined, '', undefined));
    expect(toDrawerScope('edge-7', 'ada', 't1')).toBe('edge-7/ada/t1');
  });
});
