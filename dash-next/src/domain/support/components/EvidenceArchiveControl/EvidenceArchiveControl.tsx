import { Button, LucideIconFileArchive, Tooltip } from '@ctrliq/quantic-react';

import { useCanWrite } from '~/domain/auth/hooks/useCanWrite';
import { useServerConfig } from '~/shared/context/ConfigContext';

import { useArchiveFlows } from '../../hooks/useArchiveFlows';
import type { ArchiveCluster, ArchiveFlows } from '../../types/archive';
import { toControlCaption } from '../../utils/formatters';
import { isArchiveOffered, isCollectable } from '../../utils/predicates';
import { flowFor } from '../../utils/selectors';
import styles from './EvidenceArchiveControl.module.scss';

type Props = {
  cluster: ArchiveCluster;
};

const noFlows: ArchiveFlows = { flows: [], onPressed: () => undefined };

const ArchiveButton = ({ cluster }: Props) => {
  const { flows, onPressed } = useArchiveFlows() ?? noFlows;
  const caption = toControlCaption(cluster);
  return (
    <Tooltip>
      <span className={styles.trigger}>
        <Button
          variant="ghost"
          size="sm"
          icon
          disabled={!isCollectable(cluster) || flowFor(flows, cluster) !== undefined}
          onClick={() => {
            onPressed(cluster);
          }}
        >
          <LucideIconFileArchive size="sm" />
          <span className="sr-only">{caption}</span>
        </Button>
      </span>
      <span slot="content">{caption}</span>
    </Tooltip>
  );
};

export const EvidenceArchiveControl = ({ cluster }: Props) => {
  const isOffered = isArchiveOffered(useServerConfig(), useCanWrite());
  return isOffered ? <ArchiveButton cluster={cluster} /> : null;
};
