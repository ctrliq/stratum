export const loginPath = '/login';
export const rootPath = '/';
export const cliAuthorizePath = '/cli/authorize';
export const oidcStartPath = '/api/v1/auth/oidc/start';

export const sessionEndpoints = ['/api/v1/me', '/api/v1/login', '/api/v1/logout'];
export const sessionFreeQueries = ['api/v1/config', 'api/v1/me'];

export const tokenProvider = 'token';
export const oidcProvider = 'oidc';
export const adminRole = 'admin';

export const dashboardCsrfHeader = 'X-Stratum-CSRF';
export const hubCsrfHeader = 'X-Fleet-CSRF';
export const defaultKubernetesBase = '/k8s';
export const clusterPlaceholder = '{id}';

export const unauthorized = 401;
export const confirmationDeadlineMs = 5000;

export const sessionScope = { id: 'session' };
export const signInMutationKey = ['auth', 'signIn'];
export const sessionMarkerKey = 'stratum-dashboard-session-generation';

export const returnTargets = ['cli-authorize'] as const;

export const ssoFailures = {
  oidc_failed: 'SSO sign-in failed. Try again or contact your administrator.',
  oidc_unavailable: 'SSO is unavailable. Try again later or use your local account.',
} as const;

export const signInRefusals: Record<string, string> = {
  invalid_token: 'The cluster did not accept that token.',
  invalid_credentials: 'Invalid username or password.',
};

export const unreachableDashboard = 'Cannot reach the dashboard server.';
export const unreachableHub = 'Cannot reach the fleet manager.';
export const missingIdentity =
  'The server accepted the sign-in, but the signed-in user could not be established.';
export const signOutFailed = 'Signing out failed. You are still signed in.';
export const sessionUnavailable = 'The dashboard could not check who is signed in.';
