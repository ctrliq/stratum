import {
  Button,
  ButtonBar,
  ButtonBarItem,
  LucideIconContainer,
  LucideIconServer,
  LucideIconX,
} from '@ctrliq/quantic-react';
import { type CSSProperties, type PointerEvent, useState } from 'react';

import { hostShellCaption, largestDrawerHeight, smallestDrawerHeight } from '../../constants';
import type { DrawerTab, PodTab, TabMode } from '../../types/terminal';
import { hasPane } from '../../utils/predicates';
import { toDrawerHeight } from '../../utils/reducers';
import { shownModeOf } from '../../utils/selectors';
import { ExecTerminal } from '../ExecTerminal/ExecTerminal';
import { LogViewer } from '../LogViewer/LogViewer';
import { NodeShell } from '../NodeShell/NodeShell';
import styles from './TerminalDrawer.module.scss';

type Props = {
  tabs: DrawerTab[];
  active: DrawerTab;
  canWrite: boolean;
  height: number;
  onHeightChanged: (height: number) => void;
  onTabSelected: (id: string) => void;
  onTabClosed: (id: string) => void;
  onModeChosen: (id: string, mode: TabMode) => void;
  onAllClosed: () => void;
};

type Drag = {
  startHeight: number;
  startY: number;
};

type PodPanesProps = {
  tab: PodTab;
  canWrite: boolean;
};

function PodPanes({ tab, canWrite }: PodPanesProps) {
  const mode = shownModeOf(tab, canWrite);
  return (
    <>
      {hasPane(tab, 'logs', canWrite) ? (
        <div className={styles.pane} hidden={mode !== 'logs'}>
          <LogViewer pod={tab} />
        </div>
      ) : null}
      {hasPane(tab, 'exec', canWrite) ? (
        <div className={styles.pane} hidden={mode !== 'exec'}>
          <ExecTerminal
            target={{ namespace: tab.namespace, pod: tab.name, container: tab.containers[0] }}
          />
        </div>
      ) : null}
    </>
  );
}

export function TerminalDrawer({
  tabs,
  active,
  canWrite,
  height,
  onHeightChanged,
  onTabSelected,
  onTabClosed,
  onModeChosen,
  onAllClosed,
}: Props) {
  const [drag, setDrag] = useState<Drag>();
  const mode = active.type === 'pod' ? shownModeOf(active, canWrite) : undefined;

  const onDragStarted = (event: PointerEvent<HTMLDivElement>): void => {
    event.currentTarget.setPointerCapture(event.pointerId);
    setDrag({ startHeight: height, startY: event.clientY });
  };

  const onDragged = (event: PointerEvent<HTMLDivElement>): void => {
    if (drag) {
      onHeightChanged(toDrawerHeight(drag.startHeight, drag.startY, event.clientY));
    }
  };

  return (
    <section
      className={styles.main}
      aria-label="Logs and terminals"
      style={{ '--drawer-height': `${height}px` } as CSSProperties}
    >
      <div
        className={styles.handle}
        role="separator"
        aria-orientation="horizontal"
        aria-label="Resize panel"
        aria-valuenow={height}
        aria-valuemin={smallestDrawerHeight}
        aria-valuemax={largestDrawerHeight}
        onPointerDown={onDragStarted}
        onPointerMove={onDragged}
        onPointerUp={() => {
          setDrag(undefined);
        }}
        onPointerCancel={() => {
          setDrag(undefined);
        }}
      />
      <header className={styles.bar}>
        <ul className={styles.tabs} aria-label="Open sessions">
          {tabs.map((tab) => (
            <li key={tab.id} className={styles.tab}>
              <Button
                variant={tab.id === active.id ? 'secondary' : 'ghost'}
                size="sm"
                aria-current={tab.id === active.id ? 'true' : undefined}
                onClick={() => {
                  onTabSelected(tab.id);
                }}
              >
                {tab.type === 'pod' ? (
                  <LucideIconContainer size="sm" />
                ) : (
                  <LucideIconServer size="sm" />
                )}
                {tab.name}
              </Button>
              <Button
                variant="ghost"
                size="sm"
                icon
                title="Close tab"
                onClick={() => {
                  onTabClosed(tab.id);
                }}
              >
                <LucideIconX size="sm" />
                <span className="sr-only">Close {tab.name}</span>
              </Button>
            </li>
          ))}
        </ul>
        {mode === undefined ? (
          <code className={styles.caption}>{hostShellCaption}</code>
        ) : (
          <ButtonBar size="sm" aria-label="View">
            <ButtonBarItem
              active={mode === 'logs'}
              onClick={() => {
                onModeChosen(active.id, 'logs');
              }}
            >
              Logs
            </ButtonBarItem>
            {canWrite ? (
              <ButtonBarItem
                active={mode === 'exec'}
                onClick={() => {
                  onModeChosen(active.id, 'exec');
                }}
              >
                Exec
              </ButtonBarItem>
            ) : null}
          </ButtonBar>
        )}
        <Button variant="ghost" size="sm" icon title="Close panel" onClick={onAllClosed}>
          <LucideIconX size="sm" />
          <span className="sr-only">Close panel</span>
        </Button>
      </header>
      {tabs.map((tab) => (
        <div key={tab.id} className={styles.pane} hidden={tab.id !== active.id}>
          {tab.type === 'pod' ? (
            <PodPanes tab={tab} canWrite={canWrite} />
          ) : (
            <NodeShell node={tab.name} />
          )}
        </div>
      ))}
    </section>
  );
}
