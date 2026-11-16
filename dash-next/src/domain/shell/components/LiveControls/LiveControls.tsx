import { IconSize } from '@ctrliq/quantic-components';
import {
  LucideIconPause,
  LucideIconRadio,
  LucideIconRefreshCw,
  NavbarMenuItem,
} from '@ctrliq/quantic-react';
import { useContext, useEffect, useState } from 'react';

import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';

import { useManualRefresh } from '../../hooks/useManualRefresh';
import type { ClusterRead } from '../../types/screen';
import { toLiveLabel } from '../../utils/formatters';
import { refreshedAtOf, staleListsOf } from '../../utils/selectors';

type Props = {
  reads: ClusterRead[];
  onLiveToggled: () => void;
};

const tickMs = 1000;

export const LiveControls = ({ reads, onLiveToggled }: Props) => {
  const isLive = useContext(LiveRefreshContext);
  const onRefreshed = useManualRefresh();
  const [now, setNow] = useState(Date.now);
  const label = toLiveLabel({
    isLive,
    refreshedAt: refreshedAtOf(reads),
    staleLists: staleListsOf(reads),
    now,
  });

  useEffect(() => {
    const ticking = setInterval(() => {
      setNow(Date.now());
    }, tickMs);
    return () => {
      clearInterval(ticking);
    };
  }, []);

  return (
    <>
      <NavbarMenuItem aria-label={label} title={label} selected={isLive} onClick={onLiveToggled}>
        {isLive ? (
          <LucideIconRadio slot="icon" size={IconSize.Small} />
        ) : (
          <LucideIconPause slot="icon" size={IconSize.Small} />
        )}
      </NavbarMenuItem>
      <NavbarMenuItem aria-label="Refresh" title="Refresh" onClick={onRefreshed}>
        <LucideIconRefreshCw slot="icon" size={IconSize.Small} />
      </NavbarMenuItem>
    </>
  );
};
