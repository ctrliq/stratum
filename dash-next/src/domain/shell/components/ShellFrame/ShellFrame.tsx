import { IconSize } from '@ctrliq/quantic-components';
import { Layout, LucideIconSearch, Navbar, NavbarMenuItem, Page } from '@ctrliq/quantic-react';
import { type ReactNode, useCallback, useEffect, useState } from 'react';

import { useCanWrite } from '~/domain/auth/hooks/useCanWrite';
import { useServerConfig } from '~/shared/context/ConfigContext';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { NamespaceContext } from '~/shared/context/NamespaceContext';
import { PaneHostContext } from '~/shared/context/PaneHostContext';
import { WriteAccessContext } from '~/shared/context/WriteAccessContext';

import { useClusterReads } from '../../hooks/useClusterReads';
import { useFleet } from '../../hooks/useFleet';
import { PaletteActionsContext } from '../../hooks/usePaletteActions';
import { useScreenPlace } from '../../hooks/useScreenPlace';
import type { PaletteAction } from '../../types/screen';
import { isFirstLoad, isPaletteShortcut, isReconnecting } from '../../utils/predicates';
import { AppLogo } from '../AppLogo/AppLogo';
import { CommandPalette } from '../CommandPalette/CommandPalette';
import { LiveControls } from '../LiveControls/LiveControls';
import { NamespaceFilter } from '../NamespaceFilter/NamespaceFilter';
import { ReconnectingBanner } from '../ReconnectingBanner/ReconnectingBanner';
import { ShellBreadcrumbs } from '../ShellBreadcrumbs/ShellBreadcrumbs';
import { ShellSidebar } from '../ShellSidebar/ShellSidebar';
import { ConnectingSplash } from '../Splash/ConnectingSplash';
import { UserMenu } from '../UserMenu/UserMenu';
import styles from './ShellFrame.module.scss';

type Props = {
  children: ReactNode;
};

type ChromeProps = Props & {
  isLive: boolean;
  onLiveToggled: () => void;
};

const ShellChrome = ({ children, isLive, onLiveToggled }: ChromeProps) => {
  const [isPaletteOpen, setIsPaletteOpen] = useState(false);
  const [actions, setActions] = useState<PaletteAction[]>([]);
  const onActionOffered = useCallback((action: PaletteAction) => {
    setActions((current) => [...current, action]);
    return () => {
      setActions((current) => current.filter((offered) => offered !== action));
    };
  }, []);
  const config = useServerConfig();
  const fleet = useFleet();
  const reads = useClusterReads();
  const { clusterId, location } = useScreenPlace();
  const [hasLoadedOnce, setHasLoadedOnce] = useState(false);
  const [paneHost, setPaneHost] = useState<HTMLElement | null>(null);

  if (!hasLoadedOnce && reads.some(({ hasData }) => hasData)) {
    setHasLoadedOnce(true);
  }

  useEffect(() => {
    const onKeyPressed = (event: KeyboardEvent): void => {
      if (isPaletteShortcut(event)) {
        event.preventDefault();
        setIsPaletteOpen((isOpen) => !isOpen);
      }
    };
    window.addEventListener('keydown', onKeyPressed);
    return () => {
      window.removeEventListener('keydown', onKeyPressed);
    };
  }, []);

  return fleet.status === 'loading' ? (
    <ConnectingSplash />
  ) : (
    <>
      <Layout ref={setPaneHost}>
        <AppLogo />
        <ShellBreadcrumbs fleet={fleet} />
        {isReconnecting(config, isLive, clusterId, reads) ? <ReconnectingBanner /> : null}
        <Navbar slot="navbar">
          {clusterId === undefined ? null : <NamespaceFilter />}
          <NavbarMenuItem
            aria-label="Search screens"
            onClick={() => {
              setIsPaletteOpen(true);
            }}
          >
            <LucideIconSearch slot="icon" size={IconSize.Small} />
          </NavbarMenuItem>
          <LiveControls reads={reads} onLiveToggled={onLiveToggled} />
          <UserMenu fleet={fleet} />
        </Navbar>
        <ShellSidebar fleet={fleet} />
        <Page slot="body">
          {location.clusterId !== undefined && isLive && !hasLoadedOnce && isFirstLoad(reads) ? (
            <p role="status" className={styles.loadingHint}>
              Loading {location.clusterId}…
            </p>
          ) : null}
          <PaneHostContext.Provider value={paneHost}>
            <PaletteActionsContext.Provider value={onActionOffered}>
              {children}
            </PaletteActionsContext.Provider>
          </PaneHostContext.Provider>
        </Page>
      </Layout>
      <CommandPalette
        fleet={fleet}
        actions={actions}
        isOpen={isPaletteOpen}
        onClosed={() => {
          setIsPaletteOpen(false);
        }}
      />
    </>
  );
};

export const ShellFrame = ({ children }: Props) => {
  const [isLive, setIsLive] = useState(true);
  const canWrite = useCanWrite();
  const { namespace } = useScreenPlace();

  return (
    <LiveRefreshContext.Provider value={isLive}>
      <NamespaceContext.Provider value={namespace}>
        <WriteAccessContext.Provider value={canWrite}>
          <ShellChrome
            isLive={isLive}
            onLiveToggled={() => {
              setIsLive((isLiveNow) => !isLiveNow);
            }}
          >
            {children}
          </ShellChrome>
        </WriteAccessContext.Provider>
      </NamespaceContext.Provider>
    </LiveRefreshContext.Provider>
  );
};
