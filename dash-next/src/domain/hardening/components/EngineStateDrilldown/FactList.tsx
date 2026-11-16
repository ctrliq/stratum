import type { LabelledValue } from '../../types/engineState';
import styles from './FactList.module.scss';

type Props = {
  facts: LabelledValue[];
};

export const FactList = ({ facts }: Props) => (
  <dl className={styles.facts}>
    {facts.map(({ label, value }) => (
      <div key={label} className={styles.fact}>
        <dt>{label}</dt>
        <dd>{value}</dd>
      </div>
    ))}
  </dl>
);
