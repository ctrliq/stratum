import '@xterm/xterm/css/xterm.css';

import { Alert } from '@ctrliq/quantic-react';

import type { ExecTarget } from '~/shared/streaming/types';

import { terminalUnavailable } from '../../constants';
import { useExecSession } from '../../hooks/useExecSession';
import { toSessionLabel } from '../../utils/formatters';
import { SessionHeader } from '../SessionHeader/SessionHeader';
import styles from './ExecTerminal.module.scss';

type Props = {
  target: ExecTarget;
};

function ExecSession({ target }: Props) {
  const { screenRef, status, hasTerminal } = useExecSession(target);

  return (
    <section className={styles.main} aria-label={`Terminal ${toSessionLabel(target)}`}>
      <SessionHeader status={status} label={toSessionLabel(target)} />
      <div ref={screenRef} className={styles.screen} />
      {hasTerminal ? null : <Alert type="error">{terminalUnavailable}</Alert>}
    </section>
  );
}

export function ExecTerminal({ target }: Props) {
  return <ExecSession key={toSessionLabel(target)} target={target} />;
}
