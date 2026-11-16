export type Backend = {
  csrfHeader: string;
  csrfToken: string;
  k8sBase: string;
  sessionRevision: number;
  onUnauthorized: (url: string) => void;
};

const state: { current: Backend } = {
  current: {
    csrfHeader: '',
    csrfToken: '',
    k8sBase: '/k8s',
    sessionRevision: 0,
    onUnauthorized: () => undefined,
  },
};

export const currentBackend = (): Backend => state.current;

export const withBackend = (patch: Partial<Backend>): Backend => {
  state.current = { ...state.current, ...patch };
  return state.current;
};

export const withSessionChanged = (): Backend =>
  withBackend({ sessionRevision: state.current.sessionRevision + 1 });
