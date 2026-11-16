import { IconSize } from '@ctrliq/quantic-components';
import { BrandIconStratumSolid, LayoutLogo } from '@ctrliq/quantic-react';

import { isDashboardServer } from '~/domain/auth/utils/predicates';
import { useServerConfig } from '~/shared/context/ConfigContext';

import { hubProductName, productName } from '../../constants';
import styles from './AppLogo.module.scss';

export const AppLogo = () => {
  const name = isDashboardServer(useServerConfig()) ? productName : hubProductName;
  return (
    <LayoutLogo slot="logo">
      <p slot="expanded" className={styles.wordmark}>
        <BrandIconStratumSolid size={IconSize.Large} aria-hidden="true" />
        {name}
      </p>
      <BrandIconStratumSolid slot="collapsed" size={IconSize.Large} aria-label={name} />
    </LayoutLogo>
  );
};
