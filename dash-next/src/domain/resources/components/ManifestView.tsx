import { ButtonBar, ButtonBarItem, CodeInput } from '@ctrliq/quantic-react';
import { useState } from 'react';

import type { KubeObject } from '~/shared/types/kubernetes';
import { toJson, toLineCount, toYaml } from '~/shared/utils/formatters';
import { withoutManagedFields } from '~/shared/utils/transforms';

import styles from './ManifestView.module.scss';

type Format = 'yaml' | 'json';

type Props = {
  object: KubeObject;
  label: string;
};

const formats: { format: Format; label: string }[] = [
  { format: 'yaml', label: 'YAML' },
  { format: 'json', label: 'JSON' },
];

const writtenAs: Record<Format, (value: unknown) => string> = {
  yaml: toYaml,
  json: toJson,
};

export function ManifestView({ object, label }: Props) {
  const [chosen, setChosen] = useState<Format>('yaml');
  const text = writtenAs[chosen](withoutManagedFields(object));

  return (
    <section className={styles.main} aria-label="Object">
      <header className={styles.toolbar}>
        <h3 className={styles.title}>Object</h3>
        <p className={styles.lineCount}>{toLineCount(text)}</p>
        <ButtonBar size="sm" aria-label="Manifest format">
          {formats.map(({ format, label }) => (
            <ButtonBarItem
              key={format}
              active={format === chosen}
              onClick={() => setChosen(format)}
            >
              {label}
            </ButtonBarItem>
          ))}
        </ButtonBar>
      </header>
      <CodeInput
        className={styles.manifest}
        aria-label={label}
        value={text}
        readOnly
        lineNumbers
        stretch
      />
    </section>
  );
}
