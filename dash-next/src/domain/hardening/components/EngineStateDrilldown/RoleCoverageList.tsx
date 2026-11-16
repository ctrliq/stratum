import { Tooltip } from '@ctrliq/quantic-react';

import { roleCoverageHint } from '../../constants';
import type { RoleCoverage } from '../../types/engineState';
import { toRoleCoverageText } from '../../utils/formatters';
import styles from './RoleCoverageList.module.scss';

type Props = {
  roles: RoleCoverage[];
};

export const RoleCoverageList = ({ roles }: Props) => (
  <ul className={styles.roles}>
    {roles.map((coverage) => (
      <li key={coverage.role}>
        <strong>{coverage.role}</strong>
        <Tooltip>
          <span className={styles.counts}>{toRoleCoverageText(coverage)}</span>
          <span slot="content">{roleCoverageHint}</span>
        </Tooltip>
      </li>
    ))}
  </ul>
);
