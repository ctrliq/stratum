import { Select } from '@ctrliq/quantic-react';

import { severities } from '~/domain/clusters/constants';

import { allNodes, allSeverities, allSignals } from '../../constants';
import type { FindingFilters as Filters, FindingView } from '../../types/engineState';
import { nodesOf, signalsOf } from '../../utils/selectors';
import { toOptions } from '../../utils/transforms';
import styles from './FindingFilters.module.scss';

type Props = {
  filters: Filters;
  findings: FindingView[];
  onFiltersChanged: (filters: Filters) => void;
};

export const FindingFilters = ({ filters, findings, onFiltersChanged }: Props) => (
  <search className={styles.filters}>
    <Select
      aria-label="Signal"
      value={filters.signal}
      options={toOptions(signalsOf(findings, filters.signal), allSignals)}
      onValueChanged={(event) => {
        onFiltersChanged({ ...filters, signal: event.detail.value });
      }}
    />
    <Select
      aria-label="Node"
      value={filters.nodeUID}
      options={toOptions(nodesOf(findings, filters.nodeUID), allNodes)}
      onValueChanged={(event) => {
        onFiltersChanged({ ...filters, nodeUID: event.detail.value });
      }}
    />
    <Select
      aria-label="Minimum severity"
      value={filters.minimumSeverity}
      options={toOptions(severities, allSeverities)}
      onValueChanged={(event) => {
        onFiltersChanged({ ...filters, minimumSeverity: event.detail.value });
      }}
    />
  </search>
);
