import type { UseQueryOptions, UseQueryResult } from '@tanstack/react-query';
import type { ReactNode } from 'react';

import type { DataTableColumns } from '~/shared/components/DataTable/DataTable';
import type { KubeObject } from '~/shared/types/kubernetes';

export type ResourceKind = {
  apiVersion: string;
  kind: string;
  plural: string;
  isNamespaced: boolean;
};

export type WriteTarget = {
  namespace: string;
  name: string;
};

export type ResourceRow<TObject extends KubeObject, TCells> = TCells & {
  uid: string | undefined;
  name: string;
  namespace: string | undefined;
  age: string;
  object: TObject;
};

export type ResourceColumns<TObject extends KubeObject, TCells> = DataTableColumns<
  ResourceRow<TObject, TCells>
>;

export type ResourceRead<TObject extends KubeObject> = Pick<
  UseQueryResult<{ items: TObject[] }, unknown>,
  'data' | 'dataUpdatedAt' | 'failureCount' | 'failureReason' | 'refetch'
>;

export type ResourceAction<TObject extends KubeObject> = {
  title: string;
  icon: ReactNode;
  isWrite: boolean;
  onRun: (object: TObject) => void;
};

export type ResourceScale<TObject extends KubeObject> = {
  replicasOf: (object: TObject) => number | undefined;
  onScale: (target: WriteTarget, replicas: number) => Promise<unknown>;
};

export type ResourceWrites<TObject extends KubeObject, TManifest extends KubeObject = TObject> = {
  onCreate: (namespace: string, manifest: TManifest) => Promise<unknown>;
  onReplace: (target: WriteTarget, manifest: TManifest) => Promise<unknown>;
  onDelete: (target: WriteTarget) => Promise<unknown>;
  onRestart?: (target: WriteTarget) => Promise<unknown>;
  scale?: ResourceScale<TObject>;
};

export type OpenedQuery<TOpened extends KubeObject> = (
  target: WriteTarget,
) => Pick<UseQueryOptions<TOpened>, 'queryKey' | 'queryFn'>;

export type BoundAction = {
  title: string;
  onRun: () => void;
};

export type ManifestResult<TManifest extends KubeObject = KubeObject> =
  { isValid: true; manifest: TManifest } | { isValid: false; notice: string };

export type DetailMode = 'view' | 'edit' | 'scale';

export type Panel<TObject extends KubeObject> =
  | { type: 'none' }
  | { type: 'detail'; object: TObject; age: string }
  | { type: 'create'; skeleton: string };

export type Confirmation<TObject extends KubeObject> =
  { type: 'none' } | { type: 'delete'; object: TObject } | { type: 'restart'; object: TObject };
