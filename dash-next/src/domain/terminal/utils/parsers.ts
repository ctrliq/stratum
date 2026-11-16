import type { ExecTarget } from '~/shared/streaming/types';

export const fromPodReference = (reference: string): Pick<ExecTarget, 'namespace' | 'pod'> => {
  const [namespace = '', pod = ''] = reference.split('/');
  return { namespace, pod };
};
