import { IconSize } from '@ctrliq/quantic-components';
import {
  LucideIconCheck,
  LucideIconTriangleAlert,
  Panel,
  PanelHeader,
} from '@ctrliq/quantic-react';
import clsx from 'clsx';

import { observedTitle } from '../../../constants';
import type { PostureSignal } from '../../../types';
import styles from './ObservedSignals.module.scss';

type Props = {
  caption: string;
  signals: PostureSignal[];
};

export const ObservedSignals = ({ caption, signals }: Props) => (
  <Panel>
    <PanelHeader slot="header">
      {observedTitle}
      <span slot="subtitle">{caption}</span>
    </PanelHeader>
    <ul className={styles.signals}>
      {signals.map(({ label, isOk, detail }) => (
        <li key={label} className={styles.signal}>
          <span
            role="img"
            aria-label={isOk ? 'ok' : 'warning'}
            className={clsx(styles.mark, { [styles.stateWarning]: !isOk })}
          >
            {isOk ? (
              <LucideIconCheck size={IconSize.Small} />
            ) : (
              <LucideIconTriangleAlert size={IconSize.Small} />
            )}
          </span>
          <strong>{label}</strong>
          <span className={styles.detail}>{detail}</span>
        </li>
      ))}
    </ul>
  </Panel>
);
