import { describe, expect, it } from 'vitest';

import type { DrawerState, Helper, PodTarget } from '../types/terminal';
import {
  asRestarted,
  asStopped,
  emptyDrawer,
  toDrawerHeight,
  toLogView,
  withAllClosed,
  withArrived,
  withDialog,
  withFailure,
  withHelper,
  withModeChosen,
  withNodeOpened,
  withOption,
  withoutHelper,
  withPodOpened,
  withScope,
  withTabClosed,
  withTabSelected,
} from './reducers';

const web: PodTarget = { namespace: 'shop', name: 'web-1', containers: ['app'] };
const database: PodTarget = { namespace: 'shop', name: 'db-0', containers: ['postgres'] };

const opened = (pods: PodTarget[]): DrawerState =>
  pods.reduce((state, pod) => withPodOpened(state, pod, 'logs'), emptyDrawer);

const idsOf = ({ tabs }: DrawerState): string[] => tabs.map(({ id }) => id);

describe('withPodOpened', () => {
  it('adds an active tab on the asked view', () => {
    expect(withPodOpened(emptyDrawer, web, 'logs')).toMatchObject({
      activeId: 'pod:shop/web-1',
      tabs: [{ type: 'pod', name: 'web-1', mode: 'logs', started: ['logs'] }],
    });
  });

  it('reuses the tab of a pod, switches its view and keeps what was started', () => {
    const state = withPodOpened(opened([web, database]), web, 'exec');
    expect([idsOf(state), state.activeId, state.tabs[0]]).toMatchObject([
      ['pod:shop/web-1', 'pod:shop/db-0'],
      'pod:shop/web-1',
      { mode: 'exec', started: ['logs', 'exec'] },
    ]);
  });

  it('separates pods of the same name in different namespaces', () => {
    expect(idsOf(opened([web, { ...web, namespace: 'test' }]))).toEqual([
      'pod:shop/web-1',
      'pod:test/web-1',
    ]);
  });

  it('does nothing without a namespace or a name', () => {
    expect([
      withPodOpened(emptyDrawer, undefined, 'logs'),
      withPodOpened(emptyDrawer, { ...web, namespace: '' }, 'logs'),
      withPodOpened(emptyDrawer, { ...web, name: '' }, 'exec'),
    ]).toEqual([emptyDrawer, emptyDrawer, emptyDrawer]);
  });

  it('keeps the container list the tab was opened with', () => {
    const state = withPodOpened(opened([web]), { ...web, containers: ['app', 'sidecar'] }, 'logs');
    expect(state.tabs[0]).toMatchObject({ containers: ['app'] });
  });
});

describe('withNodeOpened', () => {
  it('adds a host shell tab and reuses it when opened again', () => {
    const state = withNodeOpened(withNodeOpened(opened([web]), 'worker-1'), 'worker-1');
    expect([idsOf(state), state.activeId]).toEqual([
      ['pod:shop/web-1', 'node:worker-1'],
      'node:worker-1',
    ]);
  });

  it('does nothing without a node name', () => {
    expect(withNodeOpened(emptyDrawer, '')).toBe(emptyDrawer);
  });
});

describe('tabs', () => {
  const three = opened([web, database, { ...web, name: 'cache-0' }]);

  it('select the asked tab', () => {
    expect(withTabSelected(three, 'pod:shop/db-0').activeId).toBe('pod:shop/db-0');
  });

  it('activate the last remaining tab when the active one closes', () => {
    const state = withTabClosed(withTabSelected(three, 'pod:shop/db-0'), 'pod:shop/db-0');
    expect([idsOf(state), state.activeId]).toEqual([
      ['pod:shop/web-1', 'pod:shop/cache-0'],
      'pod:shop/cache-0',
    ]);
  });

  it('leave the active tab alone when another closes', () => {
    expect(withTabClosed(three, 'pod:shop/web-1').activeId).toBe('pod:shop/cache-0');
  });

  it('leave nothing active when the last one closes', () => {
    expect(withTabClosed(opened([web]), 'pod:shop/web-1')).toMatchObject({
      tabs: [],
      activeId: undefined,
    });
  });

  it('all close at once', () => {
    expect(withAllClosed(three)).toMatchObject({ tabs: [], activeId: undefined });
  });

  it('switch view and remember both were started', () => {
    const state = withModeChosen(
      withModeChosen(opened([web]), 'pod:shop/web-1', 'exec'),
      'pod:shop/web-1',
      'logs',
    );
    expect(state.tabs[0]).toMatchObject({ mode: 'logs', started: ['logs', 'exec'] });
  });
});

describe('withDialog', () => {
  it('opens and closes the shell dialog', () => {
    expect([
      withDialog(emptyDrawer, true).isDialogOpen,
      withDialog(emptyDrawer, false).isDialogOpen,
    ]).toEqual([true, false]);
  });
});

describe('toDrawerHeight', () => {
  it('follows the pointer between 140 and 700 pixels', () => {
    expect([
      toDrawerHeight(300, 500, 400),
      toDrawerHeight(300, 500, 560),
      toDrawerHeight(300, 500, 900),
      toDrawerHeight(300, 500, -400),
    ]).toEqual([400, 240, 140, 700]);
  });
});

describe('log view', () => {
  const view = toLogView(['app', 'sidecar']);

  it('starts following the first container without timestamps or previous', () => {
    expect(view).toEqual({
      container: 'app',
      isFollowing: true,
      hasTimestamps: false,
      isPrevious: false,
      lines: [],
      isStreaming: true,
      run: 0,
    });
  });

  it('names no container when none is known', () => {
    expect(toLogView([]).container).toBeUndefined();
  });

  it.each([
    [{ hasTimestamps: true }],
    [{ isPrevious: true }],
    [{ isFollowing: false }],
    [{ container: 'sidecar' }],
  ])('clears and restarts when %o changes while streaming', (option) => {
    expect(withOption(withArrived(view, ['old']), option)).toEqual({
      ...view,
      ...option,
      lines: [],
      run: 1,
    });
  });

  it('clears and restarts when another container is selected while stopped', () => {
    const stopped = asStopped(withArrived(view, ['from app']));
    expect(withOption(stopped, { container: 'sidecar' })).toEqual({
      ...view,
      container: 'sidecar',
      lines: [],
      isStreaming: true,
      run: 1,
    });
  });

  it('keeps what was read when stopped and starts over from the tail on Stream', () => {
    const stopped = asStopped(withArrived(view, ['kept']));
    expect([stopped.lines, stopped.isStreaming, asRestarted(stopped)]).toEqual([
      ['kept'],
      false,
      { ...view, run: 1 },
    ]);
  });

  it('keeps the newest 5000 lines', () => {
    const many = Array.from({ length: 5001 }, (_unused, index) => String(index));
    expect(withArrived(withArrived(view, ['first']), many).lines).toHaveLength(5000);
  });

  it('appends an error line below the lines read so far and stops', () => {
    expect(withFailure(withArrived(view, ['a']), 'HTTP 403')).toMatchObject({
      lines: ['a', '[error] HTTP 403'],
      isStreaming: false,
    });
  });
});

describe('withScope', () => {
  const opened = withPodOpened(withDialog(emptyDrawer, true), web, 'exec');
  const held = { scope: 'edge-7/ada/t1', drawer: opened };

  it('keeps the drawer while the cluster and the sign-in stay the same', () => {
    expect(withScope(held, 'edge-7/ada/t1')).toBe(held);
  });

  it.each([
    ['another cluster', 'edge-8/ada/t1'],
    ['another user', 'edge-7/bob/t2'],
    ['a new sign-in of the same user', 'edge-7/ada/t2'],
  ])('closes every tab and the shell dialog for %s', (_case, scope) => {
    expect(withScope(held, scope)).toEqual({ scope, drawer: emptyDrawer });
  });

  it('does not bring the tabs back when the first cluster is selected again', () => {
    expect(withScope(withScope(held, 'edge-8/ada/t1'), 'edge-7/ada/t1').drawer).toBe(emptyDrawer);
  });
});

describe('owned helpers', () => {
  const helper: Helper = {
    clusterId: 'local',
    node: 'worker-1',
    job: 'j1',
    selector: 'job-name=j1',
    owner: '',
  };

  it('are remembered once and forgotten by Job name', () => {
    const owned = withHelper(withHelper([], helper), helper);
    expect([owned, withoutHelper(owned, helper)]).toEqual([[helper], []]);
  });
});
