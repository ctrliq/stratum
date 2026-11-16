import { AlertType, ButtonGrow } from '@ctrliq/quantic-components';
import { Alert, Button } from '@ctrliq/quantic-react';
import type { ReactNode, SyntheticEvent } from 'react';

import styles from './SignInForm.module.scss';

type Props = {
  children: ReactNode;
  failure: string | undefined;
  isPending: boolean;
  isIncomplete: boolean;
  onSubmit: () => void;
};

export const SignInForm = ({ children, failure, isPending, isIncomplete, onSubmit }: Props) => (
  <form
    className={styles.form}
    noValidate
    onSubmit={(event: SyntheticEvent) => {
      event.preventDefault();
      onSubmit();
    }}
  >
    {children}
    {failure ? (
      <Alert type={AlertType.Error}>
        <span slot="title">Sign-in failed</span>
        {failure}
      </Alert>
    ) : null}
    <Button type="submit" grow={ButtonGrow.Always} disabled={isPending || isIncomplete}>
      {isPending ? 'Signing in…' : 'Sign in'}
    </Button>
  </form>
);
