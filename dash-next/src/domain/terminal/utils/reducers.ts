import { withLines } from '~/shared/streaming/reducers';

import { largestDrawerHeight, smallestDrawerHeight } from '../constants';
import type {
  DrawerState,
  DrawerTab,
  HeldDrawer,
  Helper,
  LogOptions,
  LogView,
  PodTab,
  PodTarget,
  TabMode,
} from '../types/terminal';
import { toErrorLine, toNodeTabId, toPodTabId } from './formatters';

export const emptyDrawer: DrawerState = { tabs: [], activeId: undefined, isDialogOpen: false };

export const withScope = (held: HeldDrawer, scope: string): HeldDrawer =>
  held.scope === scope ? held : { scope, drawer: emptyDrawer };

const withStarted = (tab: PodTab, mode: TabMode): PodTab => ({
  ...tab,
  mode,
  started: tab.started.includes(mode) ? tab.started : [...tab.started, mode],
});

const withTab = (state: DrawerState, id: string, opened: DrawerTab): DrawerState => ({
  ...state,
  activeId: id,
  tabs: state.tabs.some((tab) => tab.id === id) ? state.tabs : [...state.tabs, opened],
});

export const withModeChosen = (state: DrawerState, id: string, mode: TabMode): DrawerState => ({
  ...state,
  tabs: state.tabs.map((tab) =>
    tab.id === id && tab.type === 'pod' ? withStarted(tab, mode) : tab,
  ),
});

export const withPodOpened = (
  state: DrawerState,
  pod: PodTarget | undefined,
  mode: TabMode,
): DrawerState => {
  const id = pod ? toPodTabId(pod) : '';
  return pod?.namespace && pod.name
    ? withModeChosen(withTab(state, id, { ...pod, type: 'pod', id, mode, started: [] }), id, mode)
    : state;
};

export const withNodeOpened = (state: DrawerState, name: string): DrawerState =>
  name === ''
    ? state
    : withTab(state, toNodeTabId(name), { type: 'node', id: toNodeTabId(name), name });

export const withTabSelected = (state: DrawerState, id: string): DrawerState => ({
  ...state,
  activeId: id,
});

export const withTabClosed = (state: DrawerState, id: string): DrawerState => {
  const tabs = state.tabs.filter((tab) => tab.id !== id);
  return { ...state, tabs, activeId: state.activeId === id ? tabs.at(-1)?.id : state.activeId };
};

export const withAllClosed = (state: DrawerState): DrawerState => ({
  ...state,
  tabs: [],
  activeId: undefined,
});

export const withDialog = (state: DrawerState, isDialogOpen: boolean): DrawerState => ({
  ...state,
  isDialogOpen,
});

export const toDrawerHeight = (startHeight: number, startY: number, pointerY: number): number =>
  Math.min(largestDrawerHeight, Math.max(smallestDrawerHeight, startHeight + startY - pointerY));

export const toLogView = (containers: string[]): LogView => ({
  container: containers[0],
  isFollowing: true,
  hasTimestamps: false,
  isPrevious: false,
  lines: [],
  isStreaming: true,
  run: 0,
});

export const asRestarted = (view: LogView): LogView => ({
  ...view,
  lines: [],
  isStreaming: true,
  run: view.run + 1,
});

export const withOption = (view: LogView, option: Partial<LogOptions>): LogView =>
  asRestarted({ ...view, ...option });

export const asStopped = (view: LogView): LogView => ({ ...view, isStreaming: false });

export const withArrived = (view: LogView, lines: string[]): LogView => ({
  ...view,
  lines: withLines(view.lines, lines),
});

export const withFailure = (view: LogView, message: string): LogView =>
  asStopped(withArrived(view, [toErrorLine(message)]));

export const withHelper = (owned: Helper[], helper: Helper): Helper[] => [
  ...owned.filter(({ job }) => job !== helper.job),
  helper,
];

export const withoutHelper = (owned: Helper[], helper: Helper): Helper[] =>
  owned.filter(({ job }) => job !== helper.job);
