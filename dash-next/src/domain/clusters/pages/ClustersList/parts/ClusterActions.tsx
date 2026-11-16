import { ButtonSize, ButtonVariant, IconSize } from '@ctrliq/quantic-components';
import {
  ActionGroup,
  Button,
  LucideIconLogOut,
  LucideIconShield,
  LucideIconShieldCheck,
  LucideIconTrash,
  Tooltip,
} from '@ctrliq/quantic-react';
import { useContext } from 'react';

import { useSetClusterDeletionProtection } from '~/api/fleet/fleetComponents';
import type { Cluster } from '~/api/fleet/fleetSchemas';
import { EvidenceArchiveControl } from '~/domain/support/components/EvidenceArchiveControl/EvidenceArchiveControl';
import { toast } from '~/shared/components/Toaster/toast';
import { useServerConfig } from '~/shared/context/ConfigContext';
import { WriteAccessContext } from '~/shared/context/WriteAccessContext';
import { hasFeature } from '~/shared/utils/predicates';

import { useFleetRefresh } from '../../../hooks/useFleetRefresh';
import {
  toActionError,
  toDestroyTooltip,
  toProtectionToast,
  toProtectionTooltip,
} from '../../../utils/formatters';
import { isDestroyable } from '../../../utils/predicates';
import { protectionIn } from '../../../utils/selectors';

type Props = {
  cluster: Cluster;
  onUnregisterChosen: (cluster: Cluster) => void;
  onDestroyChosen: (cluster: Cluster) => void;
};

export const ClusterActions = ({ cluster, onUnregisterChosen, onDestroyChosen }: Props) => {
  const canManage = useContext(WriteAccessContext);
  const canProvision = hasFeature(useServerConfig(), 'capi');
  const onFleetChanged = useFleetRefresh();
  const protection = useSetClusterDeletionProtection({
    onSuccess: (answer, { body }) => {
      toast(toProtectionToast(cluster.name, protectionIn(answer, body.protected)));
      void onFleetChanged();
    },
    onError: (error) => {
      toast.error(toActionError(error));
    },
  });
  return canManage ? (
    <ActionGroup aria-label={`Actions for ${cluster.name}`}>
      <Tooltip>
        <Button
          icon
          variant={ButtonVariant.Ghost}
          size={ButtonSize.Small}
          aria-label="Unregister"
          onClick={() => {
            onUnregisterChosen(cluster);
          }}
        >
          <LucideIconLogOut size={IconSize.Small} />
        </Button>
        <span slot="content">Unregister from the fleet manager</span>
      </Tooltip>
      {canProvision && cluster.provisioned ? (
        <Tooltip>
          <Button
            icon
            variant={cluster.deletionProtected ? ButtonVariant.Secondary : ButtonVariant.Ghost}
            size={ButtonSize.Small}
            aria-label={toProtectionTooltip(cluster)}
            aria-pressed={cluster.deletionProtected}
            onClick={() => {
              protection.mutate({
                pathParams: { id: cluster.id },
                body: { protected: !cluster.deletionProtected },
              });
            }}
          >
            {cluster.deletionProtected ? (
              <LucideIconShieldCheck size={IconSize.Small} />
            ) : (
              <LucideIconShield size={IconSize.Small} />
            )}
          </Button>
          <span slot="content">{toProtectionTooltip(cluster)}</span>
        </Tooltip>
      ) : null}
      {canProvision ? (
        <Tooltip>
          <Button
            icon
            destructive
            variant={ButtonVariant.Ghost}
            size={ButtonSize.Small}
            aria-label="Destroy"
            disabled={!isDestroyable(cluster)}
            onClick={() => {
              onDestroyChosen(cluster);
            }}
          >
            <LucideIconTrash size={IconSize.Small} />
          </Button>
          <span slot="content">{toDestroyTooltip(cluster)}</span>
        </Tooltip>
      ) : null}
      <EvidenceArchiveControl cluster={cluster} />
    </ActionGroup>
  ) : null;
};
