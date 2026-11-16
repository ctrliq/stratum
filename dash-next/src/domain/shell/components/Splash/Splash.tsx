import { LayoutVariant } from '@ctrliq/quantic-components';
import { Layout, Page } from '@ctrliq/quantic-react';
import type { ReactNode } from 'react';

import styles from './Splash.module.scss';

type Props = {
  children: ReactNode;
};

export const Splash = ({ children }: Props) => (
  <Layout variant={LayoutVariant.Focused}>
    <Page slot="body" contained>
      <p role="status" className={styles.message}>
        {children}
      </p>
    </Page>
  </Layout>
);
