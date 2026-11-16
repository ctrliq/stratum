import { useContext } from 'react';

import { useGetClusterCapabilities } from '~/api/fleet/fleetComponents';
import type { Capability } from '~/api/fleet/fleetSchemas';
import { useScreenNavigation } from '~/domain/shell/hooks/useScreenNavigation';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';
import { LiveRefreshContext } from '~/shared/context/LiveRefreshContext';
import { polledWhen } from '~/shared/query/polling';

import { operationsPath } from '../../constants';
import { toDenialText, toHandlerText, toRefusalNote } from '../../utils/formatters';
import { isCapabilityAvailable } from '../../utils/predicates';
import { evidenceCapabilitiesOf, recordFailureOf } from '../../utils/selectors';
import { EngineStateNote } from '../EngineStateFacts/EngineStateFacts';
import styles from './CapabilityAvailability.module.scss';

type Props = {
  clusterId: string;
};

type EntryProps = {
  capability: Capability;
};

const CapabilityEntry = ({ capability }: EntryProps) => {
  const { onLinkClicked } = useScreenNavigation();
  const isAvailable = isCapabilityAvailable(capability);
  const handler = toHandlerText(capability);
  const denial = isAvailable ? undefined : toDenialText(capability);
  return (
    <li className={styles.entry}>
      <code>{capability.action}</code>
      <ToneBadge tone={isAvailable ? 'success' : 'warning'}>
        {isAvailable ? 'available' : 'unavailable'}
      </ToneBadge>
      {handler ? <small>{handler}</small> : null}
      {denial ? <small>{denial}</small> : null}
      {isAvailable ? (
        <a
          href={operationsPath}
          onClick={(event) => {
            onLinkClicked(event, operationsPath);
          }}
        >
          Operations
        </a>
      ) : null}
    </li>
  );
};

export const CapabilityAvailability = ({ clusterId }: Props) => {
  const isLive = useContext(LiveRefreshContext);
  const record = useGetClusterCapabilities(
    { pathParams: { id: clusterId } },
    { ...polledWhen(isLive), select: evidenceCapabilitiesOf },
  );
  const { data } = record;
  const failure = recordFailureOf(record);
  return (
    <section className={styles.main} aria-label="Capability availability">
      <h3 className={styles.heading}>Capability availability</h3>
      {failure !== undefined ? (
        <EngineStateNote>{toRefusalNote('capability record unavailable', failure)}</EngineStateNote>
      ) : data === undefined ? (
        <EngineStateNote>reading capability availability…</EngineStateNote>
      ) : data.length === 0 ? (
        <EngineStateNote>
          the record answers none of the actions these evidence panels relate to
        </EngineStateNote>
      ) : null}
      {data && data.length > 0 ? (
        <ul className={styles.entries}>
          {data.map((capability) => (
            <CapabilityEntry key={capability.action} capability={capability} />
          ))}
        </ul>
      ) : null}
    </section>
  );
};
