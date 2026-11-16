import { useRouterState } from '@tanstack/react-router';
import { useMemo } from 'react';

import { EmptyState } from '~/shared/components/EmptyState/EmptyState';
import { PageTitle } from '~/shared/components/PageTitle/PageTitle';

import { heading } from '../../constants';
import { toTrackedLine } from '../../utils/formatters';
import { fromOperationsQuery } from '../../utils/parsers';
import styles from './OperationsList.module.scss';
import { TrackedOperation } from './parts/TrackedOperation';

export function OperationsList() {
  const searchStr = useRouterState({ select: (state) => state.location.searchStr });
  const operationIds = useMemo(() => fromOperationsQuery(searchStr), [searchStr]);
  return (
    <>
      <header className={styles.header}>
        <PageTitle className={styles.title}>{heading}</PageTitle>
        <p className={styles.summary}>{toTrackedLine(operationIds.length)}</p>
      </header>
      {operationIds.length === 0 ? (
        <EmptyState>
          No operations tracked. An operation opened from a submit flow carries a link of the form{' '}
          <code>/operations?op=&lt;id&gt;</code> — follow it, or track several with repeated{' '}
          <code>op</code> parameters.
        </EmptyState>
      ) : (
        <ul className={styles.operations}>
          {operationIds.map((operationId) => (
            <li key={operationId}>
              <TrackedOperation operationId={operationId} />
            </li>
          ))}
        </ul>
      )}
    </>
  );
}
