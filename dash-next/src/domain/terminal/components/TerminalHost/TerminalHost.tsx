import { type ReactNode, useContext, useEffect, useMemo, useState } from 'react';

import { useGetMe } from '~/api/fleet/fleetComponents';
import { useSession } from '~/domain/auth/hooks/useSession';
import { csrfTokenOf, identityOf } from '~/domain/auth/utils/selectors';
import { usePaletteAction } from '~/domain/shell/hooks/usePaletteActions';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { useServerConfig } from '~/shared/context/ConfigContext';
import { WriteAccessContext } from '~/shared/context/WriteAccessContext';
import { hasFeature } from '~/shared/utils/predicates';

import { helperReleaseDeadlineMs, initialDrawerHeight, shellDialogCommand } from '../../constants';
import { TerminalContext } from '../../hooks/useTerminal';
import { ShellDialog } from '../../modals/ShellDialog';
import type { DrawerState, HeldDrawer, TerminalActions } from '../../types/terminal';
import { toDrawerScope } from '../../utils/formatters';
import {
  emptyDrawer,
  withAllClosed,
  withDialog,
  withModeChosen,
  withNodeOpened,
  withPodOpened,
  withScope,
  withTabClosed,
  withTabSelected,
} from '../../utils/reducers';
import { activeTabOf, shownTabsOf } from '../../utils/selectors';
import { releasedWithin } from '../NodeShell/ownedHelpers';
import { TerminalDrawer } from '../TerminalDrawer/TerminalDrawer';
import styles from './TerminalHost.module.scss';

type Props = {
  children: ReactNode;
};

export function TerminalHost({ children }: Props) {
  const clusterId = useContext(ClusterContext);
  const canWrite = useContext(WriteAccessContext);
  const config = useServerConfig();
  const username = identityOf(useSession().session)?.username ?? '';
  const signIn = useGetMe({}, { enabled: false, select: csrfTokenOf });
  const scope = toDrawerScope(clusterId, username, signIn.data);
  const [height, setHeight] = useState(initialDrawerHeight);
  const [stored, setHeld] = useState<HeldDrawer>({ scope, drawer: emptyDrawer });
  const held = withScope(stored, scope);
  const { drawer } = held;

  if (held !== stored) {
    setHeld(held);
  }

  const tabs = shownTabsOf(drawer, canWrite);
  const active = activeTabOf(tabs, drawer.activeId);
  const isNodeShellOffered = canWrite && hasFeature(config, 'nodeShell');

  const actions = useMemo(() => {
    const onChanged = (change: (current: DrawerState) => DrawerState): void => {
      setHeld((current) => ({ scope, drawer: change(withScope(current, scope).drawer) }));
    };
    return {
      onChanged,
      offered: {
        isExecOffered: canWrite,
        isNodeShellOffered,
        onLogsOpened: (pod) => {
          onChanged((current) => withPodOpened(current, pod, 'logs'));
        },
        onExecOpened: (pod) => {
          onChanged((current) => (canWrite ? withPodOpened(current, pod, 'exec') : current));
        },
        onNodeShellOpened: (nodeName) => {
          onChanged((current) =>
            isNodeShellOffered ? withNodeOpened(current, nodeName) : current,
          );
        },
        onShellDialogOpened: () => {
          onChanged((current) => withDialog(current, canWrite));
        },
      } satisfies TerminalActions,
    };
  }, [scope, canWrite, isNodeShellOffered, setHeld]);
  const { onShellDialogOpened } = actions.offered;

  usePaletteAction(
    useMemo(
      () =>
        canWrite && clusterId !== undefined
          ? { id: 'pod-shell', title: shellDialogCommand, onRun: onShellDialogOpened }
          : undefined,
      [canWrite, clusterId, onShellDialogOpened],
    ),
  );

  useEffect(() => {
    const onPageHidden = (): void => {
      void releasedWithin(helperReleaseDeadlineMs, { keepalive: true });
    };
    window.addEventListener('pagehide', onPageHidden);
    return () => {
      window.removeEventListener('pagehide', onPageHidden);
    };
  }, []);

  return (
    <TerminalContext.Provider value={actions.offered}>
      <div className={styles.main}>
        <div className={styles.page}>{children}</div>
        {active ? (
          <TerminalDrawer
            key={scope}
            tabs={tabs}
            active={active}
            canWrite={canWrite}
            height={height}
            onHeightChanged={setHeight}
            onTabSelected={(id) => {
              actions.onChanged((current) => withTabSelected(current, id));
            }}
            onTabClosed={(id) => {
              actions.onChanged((current) => withTabClosed(current, id));
            }}
            onModeChosen={(id, mode) => {
              actions.onChanged((current) => withModeChosen(current, id, mode));
            }}
            onAllClosed={() => {
              actions.onChanged(withAllClosed);
            }}
          />
        ) : null}
      </div>
      <ShellDialog
        key={scope}
        isOpen={drawer.isDialogOpen && canWrite}
        onClosed={() => {
          actions.onChanged((current) => withDialog(current, false));
        }}
      />
    </TerminalContext.Provider>
  );
}
