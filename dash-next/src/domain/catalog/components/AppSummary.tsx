import { Alert } from '@ctrliq/quantic-react';

import { absentText } from '~/shared/utils/formatters';

import type { CatalogApp } from '../types/catalog';
import { toParagraphs } from '../utils/transforms';
import styles from './AppSummary.module.scss';
import { ValueList } from './ValueList';

type Props = {
  app: CatalogApp;
  targetNamespace: string;
};

export const AppSummary = ({ app, targetNamespace }: Props) => {
  const paragraphs = toParagraphs(app.description || app.shortDescription);
  return (
    <>
      <div className={styles.description}>
        {paragraphs.length > 0 ? (
          paragraphs.map((paragraph) => <p key={paragraph}>{paragraph}</p>)
        ) : (
          <p>{absentText}</p>
        )}
      </div>
      <ValueList
        title="Chart"
        rows={[
          { key: 'Repository', value: app.chart.repo },
          { key: 'Chart', value: app.chart.name },
          { key: 'Version', value: app.chart.version },
          { key: 'Target namespace', value: targetNamespace },
          ...(app.install.timeout ? [{ key: 'Install timeout', value: app.install.timeout }] : []),
        ]}
      />
      {app.notes ? <Alert type="info">{app.notes}</Alert> : null}
    </>
  );
};
