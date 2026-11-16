import { Button, LucideIconX, Panel, PanelHeader } from '@ctrliq/quantic-react';

import type { VersionRow } from '../../../../types/versions';
import { toVersionFacts } from '../../../../utils/transforms';
import styles from './VersionDetail.module.scss';

type Props = {
  row: VersionRow;
  reportGeneration: string;
  onClosed: () => void;
};

export function VersionDetail({ row, reportGeneration, onClosed }: Props) {
  return (
    <Panel>
      <PanelHeader slot="header">
        Version comparison detail
        <Button slot="meta" variant="ghost" size="sm" icon title="Close" onClick={onClosed}>
          <LucideIconX size="sm" />
          <span className="sr-only">Close the version detail</span>
        </Button>
      </PanelHeader>
      <dl className={styles.facts}>
        {toVersionFacts(row, reportGeneration).map(({ label, value }, index) => (
          <div key={`${label}/${index}`} className={styles.fact}>
            <dt>{label}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      </dl>
    </Panel>
  );
}
