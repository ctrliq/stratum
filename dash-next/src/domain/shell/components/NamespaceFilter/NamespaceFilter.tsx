import { ButtonSize, ButtonVariant, DropdownMenuSize, IconSize } from '@ctrliq/quantic-components';
import {
  Button,
  DropdownMenu,
  DropdownMenuItem,
  LucideIconChevronDown,
} from '@ctrliq/quantic-react';
import { useRouter } from '@tanstack/react-router';
import { useContext } from 'react';

import { useListCoreV1Namespace } from '~/api/k8s/k8sComponents';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { polledWhen } from '~/shared/query/polling';

import { allNamespaces } from '../../constants';
import { useScreenPlace } from '../../hooks/useScreenPlace';
import { listedNamesOf } from '../../utils/selectors';
import { withNamespace } from '../../utils/transforms';

const cachedApartFromTheNamespacesList = { queryParams: {} };

export const NamespaceFilter = () => {
  const router = useRouter();
  const isLive = useContext(LiveRefreshContext);
  const { clusterId, namespace, pathname, searchStr } = useScreenPlace();
  const namespaces = useListCoreV1Namespace(cachedApartFromTheNamespacesList, {
    ...polledWhen(isLive),
    enabled: clusterId !== undefined,
    select: listedNamesOf,
  });

  return (
    <DropdownMenu
      size={DropdownMenuSize.Small}
      onItemSelected={(event) => {
        router.history.replace(withNamespace(pathname, searchStr, event.detail.value));
      }}
    >
      <Button
        slot="trigger"
        variant={ButtonVariant.Ghost}
        size={ButtonSize.Small}
        aria-label={`Namespace filter: ${namespace ?? allNamespaces}`}
      >
        {namespace ?? allNamespaces}
        <LucideIconChevronDown size={IconSize.ExtraSmall} />
      </Button>
      <DropdownMenuItem value="" selected={namespace === undefined}>
        {allNamespaces}
      </DropdownMenuItem>
      {(namespaces.data ?? []).map((name) => (
        <DropdownMenuItem key={name} value={name} selected={name === namespace}>
          {name}
        </DropdownMenuItem>
      ))}
    </DropdownMenu>
  );
};
