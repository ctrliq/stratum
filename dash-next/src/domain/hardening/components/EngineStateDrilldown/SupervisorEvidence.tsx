import type { SupervisorEvidence as Supervisor } from '../../types/evidence';
import {
  toChildLine,
  toMirrorPodLine,
  toRuntimeLine,
  toSupervisorFacts,
  toUptimeLine,
} from '../../utils/formatters';
import { FactList } from './FactList';
import styles from './SupervisorEvidence.module.scss';

type Props = {
  item: Supervisor;
  now: number;
};

export const SupervisorEvidence = ({ item, now }: Props) => {
  const uptime = toUptimeLine(item, now);
  return (
    <section className={styles.supervisor}>
      <h4>Supervisor</h4>
      <FactList facts={toSupervisorFacts(item)} />
      {uptime === undefined ? null : <p>{uptime}</p>}
      <ul className={styles.children}>
        {(item.children ?? []).map((child, index) => (
          <li key={child?.component ?? index}>{toChildLine(child)}</li>
        ))}
      </ul>
      {item.runtime ? <p>{toRuntimeLine(item.runtime, now)}</p> : null}
      {item.mirrorPod ? <p>{toMirrorPodLine(item.mirrorPod, now)}</p> : null}
    </section>
  );
};
