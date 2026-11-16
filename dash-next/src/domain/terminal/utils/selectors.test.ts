import { describe, expect, it } from 'vitest';

import { helperTimeoutMessage, missingImageMessage } from '../constants';
import type { DrawerState, HelperRead } from '../types/terminal';
import {
  activeTabOf,
  colorFor,
  helperPodsOf,
  phaseOf,
  runningPodsOf,
  selectorOf,
  shownModeOf,
  shownTabsOf,
} from './selectors';

const drawer: DrawerState = {
  activeId: 'node:worker-1',
  isDialogOpen: false,
  tabs: [
    { type: 'node', id: 'node:worker-1', name: 'worker-1' },
    {
      type: 'pod',
      id: 'pod:shop/web-1',
      namespace: 'shop',
      name: 'web-1',
      containers: ['app'],
      mode: 'exec',
      started: ['exec'],
    },
  ],
};

const read: HelperRead = { image: 'busybox', createError: undefined, pods: [], hasTimedOut: false };

describe('colorFor', () => {
  it.each([
    ['connecting', 'warning'],
    ['connected', 'success'],
    ['closed', 'gray'],
    ['error', 'error'],
    ['creating', 'warning'],
    ['waiting', 'warning'],
  ] as const)('shows %s in the %s colour', (status, colour) => {
    expect(colorFor(status)).toBe(colour);
  });
});

describe('shown tabs', () => {
  it('hide host shells from a session that cannot write', () => {
    expect([
      shownTabsOf(drawer, true).length,
      shownTabsOf(drawer, false).map(({ id }) => id),
    ]).toEqual([2, ['pod:shop/web-1']]);
  });

  it('fall back to the first tab when the active one is not shown', () => {
    expect([
      activeTabOf(shownTabsOf(drawer, false), drawer.activeId)?.id,
      activeTabOf(shownTabsOf(drawer, true), drawer.activeId)?.id,
      activeTabOf([], 'x'),
    ]).toEqual(['pod:shop/web-1', 'node:worker-1', undefined]);
  });

  it('show logs in place of exec without write permission', () => {
    expect([shownModeOf({ mode: 'exec' }, false), shownModeOf({ mode: 'exec' }, true)]).toEqual([
      'logs',
      'exec',
    ]);
  });
});

describe('selectorOf', () => {
  it('joins the selector labels of the Job', () => {
    expect(
      selectorOf({
        metadata: { name: 'j1' },
        spec: { selector: { matchLabels: { 'controller-uid': 'abc', app: 'x' } }, template: {} },
      }),
    ).toBe('controller-uid=abc,app=x');
  });

  it('falls back to the job-name label', () => {
    expect(selectorOf({ metadata: { name: 'j1' } })).toBe('job-name=j1');
  });
});

describe('phaseOf', () => {
  it('fails closed without a server-provided image', () => {
    expect([phaseOf({ ...read, image: undefined }), phaseOf({ ...read, image: '' })]).toEqual([
      { type: 'failed', message: missingImageMessage },
      { type: 'failed', message: missingImageMessage },
    ]);
  });

  it('names the Job and the answer of a refused create', () => {
    expect(
      phaseOf({ ...read, createError: { status: 403, payload: { message: 'jobs is forbidden' } } }),
    ).toEqual({ type: 'failed', message: 'create debug job → HTTP 403 jobs is forbidden' });
  });

  it('is creating until a pod exists, then waiting with its name', () => {
    expect([
      phaseOf(read),
      phaseOf({ ...read, pods: [{ name: 'shell-x', phase: 'Pending' }] }),
    ]).toEqual([{ type: 'creating' }, { type: 'waiting', pod: 'shell-x' }]);
  });

  it('is running once the pod runs, even past the wait', () => {
    expect(
      phaseOf({ ...read, hasTimedOut: true, pods: [{ name: 'shell-x', phase: 'Running' }] }),
    ).toEqual({ type: 'running', pod: 'shell-x' });
  });

  it.each(['Failed', 'Succeeded'])('fails when the pod reports %s before running', (phase) => {
    expect(phaseOf({ ...read, pods: [{ name: 'shell-x', phase }] })).toEqual({
      type: 'failed',
      message: `debug pod ${phase}`,
    });
  });

  it('times out when the pod never runs', () => {
    expect(phaseOf({ ...read, hasTimedOut: true })).toEqual({
      type: 'failed',
      message: helperTimeoutMessage,
    });
  });
});

describe('helperPodsOf', () => {
  it('reads each pod name and phase', () => {
    expect(helperPodsOf([{ metadata: { name: 'shell-x' }, status: { phase: 'Running' } }])).toEqual(
      [{ name: 'shell-x', phase: 'Running' }],
    );
  });
});

describe('runningPodsOf', () => {
  it('lists only running pods, from every namespace', () => {
    expect(
      runningPodsOf({
        items: [
          { metadata: { namespace: 'shop', name: 'web-1' }, status: { phase: 'Running' } },
          { metadata: { namespace: 'shop', name: 'web-2' }, status: { phase: 'Pending' } },
          { metadata: { namespace: 'jobs', name: 'done' }, status: { phase: 'Succeeded' } },
          { metadata: { namespace: 'test', name: 'api-0' }, status: { phase: 'Running' } },
        ],
      }),
    ).toEqual(['shop/web-1', 'test/api-0']);
  });
});
