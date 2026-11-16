import type { ObjectMeta } from '~/api/k8s/k8sSchemas';

export type KubeObject = {
  apiVersion?: string;
  kind?: string;
  metadata?: ObjectMeta;
};

export type ObjectReference = {
  namespace?: string;
  name: string;
};
