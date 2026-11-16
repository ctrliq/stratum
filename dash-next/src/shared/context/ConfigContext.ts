import { createContext, useContext } from 'react';

import type { DashboardConfig } from '~/api/dashboard/dashboardSchemas';
import type { FleetConfig } from '~/api/fleet/fleetSchemas';

export type ServerConfig = DashboardConfig | FleetConfig;

export type ServerFeature = keyof DashboardConfig['features'] | keyof FleetConfig['features'];

export const unresolvedConfig: ServerConfig = {
  mode: 'standalone',
  version: '',
  k8sBase: '/k8s',
  auth: { required: true, providers: [] },
  features: { capi: false, kubeconfigSource: false },
  links: {},
  cisProfile: [],
};

export const ConfigContext = createContext<ServerConfig>(unresolvedConfig);

export const useServerConfig = (): ServerConfig => useContext(ConfigContext);
