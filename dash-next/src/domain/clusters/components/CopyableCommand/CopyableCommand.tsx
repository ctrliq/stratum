import { ButtonSize, ButtonVariant, IconSize } from '@ctrliq/quantic-components';
import { Button, LucideIconCheck, LucideIconCopy } from '@ctrliq/quantic-react';
import { useState } from 'react';

import { copiedMarkMs } from '../../constants';
import styles from './CopyableCommand.module.scss';

type Props = {
  label: string;
  text: string;
};

export const CopyableCommand = ({ label, text }: Props) => {
  const [isCopied, setIsCopied] = useState(false);
  const onCopied = (): void => {
    void Promise.resolve()
      .then(() => navigator.clipboard.writeText(text))
      .then(
        () => {
          setIsCopied(true);
          setTimeout(() => {
            setIsCopied(false);
          }, copiedMarkMs);
        },
        () => undefined,
      );
  };
  return (
    <figure className={styles.main}>
      <figcaption className={styles.label}>{label}</figcaption>
      <pre className={styles.command}>
        <code>{text}</code>
      </pre>
      <Button
        icon
        variant={ButtonVariant.Ghost}
        size={ButtonSize.Small}
        aria-label="Copy to clipboard"
        onClick={onCopied}
      >
        {isCopied ? (
          <LucideIconCheck size={IconSize.Small} />
        ) : (
          <LucideIconCopy size={IconSize.Small} />
        )}
      </Button>
    </figure>
  );
};
