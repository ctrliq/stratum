import { AlertType, LayoutVariant } from '@ctrliq/quantic-components';
import { Alert, Layout, Page } from '@ctrliq/quantic-react';
import { type ReactNode, useEffect, useLayoutEffect } from 'react';

import { useGetConfig } from '~/api/fleet/fleetComponents';
import { toBackendPatch } from '~/domain/auth/utils/transforms';
import { ConfigContext, type ServerConfig, unresolvedConfig } from '~/shared/context/ConfigContext';
import { withBackend } from '~/shared/fetchers/backend';

import { configFailureLead, connecting } from '../../constants';
import { useReadWarnings } from '../../hooks/useReadWarnings';
import { toServerConfig } from '../../schemas/serverConfig';
import { toConfigFailure } from '../../utils/formatters';
import { Splash } from '../Splash/Splash';

type Props = {
  children: ReactNode;
};

const configRead = {
  retry: false,
  staleTime: Infinity,
  gcTime: Infinity,
  refetchOnWindowFocus: false,
  refetchOnReconnect: false,
  select: toServerConfig,
};

const loginFreeConfig = { ...unresolvedConfig, auth: { required: false, providers: [] } };

const useBackendBeforeFirstRequest = (config: ServerConfig | undefined): void => {
  useLayoutEffect(() => {
    if (config) {
      withBackend(toBackendPatch(config));
    }
  }, [config]);
};

export const ConfigGate = ({ children }: Props) => {
  const { data, error, isError } = useGetConfig({}, configRead);
  const isFallback = isError && import.meta.env.DEV;
  const config = isFallback ? loginFreeConfig : data;

  useBackendBeforeFirstRequest(config);
  useReadWarnings();

  useEffect(() => {
    if (isFallback) {
      console.warn(`${toConfigFailure(error)}; using the login-free development configuration`);
    }
  }, [isFallback, error]);

  return config ? (
    <ConfigContext.Provider value={config}>{children}</ConfigContext.Provider>
  ) : isError ? (
    <Layout variant={LayoutVariant.Focused}>
      <Page slot="body" contained>
        <Alert type={AlertType.Error}>
          <span slot="title">Configuration unavailable</span>
          {configFailureLead} {toConfigFailure(error)}
        </Alert>
      </Page>
    </Layout>
  ) : (
    <Splash>{connecting}</Splash>
  );
};
