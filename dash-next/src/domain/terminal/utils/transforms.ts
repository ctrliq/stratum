import type { Job } from '~/api/k8s/k8sSchemas';

import {
  helperAppLabel,
  helperContainer,
  helperLifetimeSeconds,
  helperNamePrefix,
  helperNodeLabel,
  helperTtlSeconds,
  noRunningPods,
} from '../constants';
import { toPodOption } from './formatters';
import { fromPodReference } from './parsers';

export const toHelperJob = (node: string, image: string): Job => {
  const labels = { app: helperAppLabel, [helperNodeLabel]: node };
  return {
    apiVersion: 'batch/v1',
    kind: 'Job',
    metadata: { generateName: helperNamePrefix, labels },
    spec: {
      ttlSecondsAfterFinished: helperTtlSeconds,
      activeDeadlineSeconds: helperLifetimeSeconds,
      backoffLimit: 0,
      template: {
        metadata: { labels },
        spec: {
          nodeName: node,
          hostPID: true,
          restartPolicy: 'Never',
          activeDeadlineSeconds: helperLifetimeSeconds,
          tolerations: [{ operator: 'Exists' }],
          containers: [
            {
              name: helperContainer,
              image,
              command: ['sleep', String(helperLifetimeSeconds)],
              stdin: true,
              tty: true,
              securityContext: { privileged: true },
            },
          ],
        },
      },
    },
  };
};

export const toPodOptions = (running: string[], selected: string): [string, string][] => [
  ...(running.length === 0
    ? [[noRunningPods, ''] satisfies [string, string]]
    : selected === ''
      ? [['', ''] satisfies [string, string]]
      : []),
  ...running.map((reference): [string, string] => {
    const { namespace, pod } = fromPodReference(reference);
    return [toPodOption({ namespace, name: pod }), reference];
  }),
];
