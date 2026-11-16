import type { DashboardLoginResponse, DashboardMe } from '~/api/dashboard/dashboardSchemas';
import type { FleetLoginResponse, FleetMe } from '~/api/fleet/fleetSchemas';

import type { returnTargets, ssoFailures } from '../constants';

export type IdentityAnswer = DashboardMe | FleetMe;

export type Identity = Omit<DashboardMe, 'csrfToken'> | Omit<FleetMe, 'csrfToken'>;

export type SessionAnswer = {
  identity: Identity | undefined;
  csrfToken: string;
};

export type Session =
  | { status: 'open' }
  | { status: 'resolving' }
  | { status: 'signedOut' }
  | { status: 'unavailable'; failure: Failure }
  | { status: 'signedIn'; identity: Identity };

export type SignInAnswer = DashboardLoginResponse | FleetLoginResponse;

export type Failure = {
  code: string;
  message: string;
};

export type ReturnTarget = (typeof returnTargets)[number];

export type SsoFailureCode = keyof typeof ssoFailures;

export type LoginSearch = {
  error?: SsoFailureCode;
  return?: ReturnTarget;
};

export type Destination = {
  href: string;
  isDocument: boolean;
};
