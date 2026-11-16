import { useServerConfig } from '~/shared/context/ConfigContext';

import { useClusterIdentity } from '../../hooks/useClusterIdentity';
import { useScreenNavigation } from '../../hooks/useScreenNavigation';
import { useScreenPlace } from '../../hooks/useScreenPlace';
import { useTrailKind } from '../../hooks/useTrailKind';
import type { Fleet } from '../../types/screen';
import { toCrumbs } from '../../utils/transforms';
import styles from './ShellBreadcrumbs.module.scss';

type Props = {
  fleet: Fleet;
};

export const ShellBreadcrumbs = ({ fleet }: Props) => {
  const config = useServerConfig();
  const { clusterId, namespace, location } = useScreenPlace();
  const { onLinkClicked } = useScreenNavigation();
  const crumbs = toCrumbs({
    config,
    clusterId,
    namespace,
    location,
    clusterName: useClusterIdentity(fleet)?.name,
    kind: useTrailKind(location),
  });

  return (
    <nav slot="titlebar" aria-label="Breadcrumb">
      <ol className={styles.trail}>
        {crumbs.map(({ label, href }, index) => (
          <li
            key={`${href ?? ''}:${label}`}
            className={styles.crumb}
            aria-current={index === crumbs.length - 1 ? 'page' : undefined}
          >
            {href ? (
              <a
                href={href}
                onClick={(event) => {
                  onLinkClicked(event, href);
                }}
              >
                {label}
              </a>
            ) : (
              label
            )}
          </li>
        ))}
      </ol>
    </nav>
  );
};
