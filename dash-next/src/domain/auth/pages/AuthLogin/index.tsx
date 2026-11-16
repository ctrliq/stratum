import { AlertType, IconSize, LayoutVariant } from '@ctrliq/quantic-components';
import { Alert, BrandIconStratumSolid, Layout, Page } from '@ctrliq/quantic-react';
import { useRouter } from '@tanstack/react-router';
import { useEffect } from 'react';

import { useServerConfig } from '~/shared/context/ConfigContext';

import { oidcProvider, ssoFailures } from '../../constants';
import { PasswordSignInForm } from '../../forms/PasswordSignInForm';
import { TokenSignInForm } from '../../forms/TokenSignInForm';
import { useSession } from '../../hooks/useSession';
import type { LoginSearch } from '../../types/session';
import { hasProvider, isDashboardServer } from '../../utils/predicates';
import { toDestination } from '../../utils/transforms';
import styles from './AuthLogin.module.scss';
import { SsoSignIn } from './parts/SsoSignIn';

type Props = {
  search: LoginSearch;
};

const tokenHeading = 'Sign in with a Kubernetes token';
const hubHeading = 'Sign in to your fleet manager';

export const AuthLogin = ({ search }: Props) => {
  const config = useServerConfig();
  const router = useRouter();
  const { session, returnTo } = useSession();
  const isSignInNeeded = session.status === 'signedOut';
  const destination = toDestination(search.return, returnTo);

  useEffect(() => {
    if (!isSignInNeeded && destination.isDocument) {
      window.location.assign(destination.href);
    } else if (!isSignInNeeded) {
      router.history.replace(destination.href);
    }
  }, [isSignInNeeded, destination.isDocument, destination.href, router]);

  return isSignInNeeded ? (
    <Layout variant={LayoutVariant.Focused}>
      <Page slot="body" contained>
        <section className={styles.signIn}>
          <BrandIconStratumSolid size={IconSize.ExtraLarge} aria-hidden="true" />
          <h1 className={styles.heading}>
            {isDashboardServer(config) ? tokenHeading : hubHeading}
          </h1>
          {search.error ? (
            <Alert type={AlertType.Error}>
              <span slot="title">Single sign-on</span>
              {ssoFailures[search.error]}
            </Alert>
          ) : null}
          {isDashboardServer(config) ? <TokenSignInForm /> : <PasswordSignInForm />}
          {hasProvider(config, oidcProvider) ? <SsoSignIn returnTarget={search.return} /> : null}
          {config.version ? <p className={styles.version}>{config.version}</p> : null}
        </section>
      </Page>
    </Layout>
  ) : null;
};
