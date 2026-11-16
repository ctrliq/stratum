import type { Operation, OperationTargetResult } from '~/api/fleet/fleetSchemas';

export type OperationAnswer = Partial<Operation> | null | undefined;

export type TargetAnswer = Partial<Omit<OperationTargetResult, 'target'>> & {
  target?: {
    cluster?: Partial<OperationTargetResult['target']['cluster']>;
    nodeUID?: string;
  };
};

export type TargetRow = {
  id: string;
  cluster: string;
  node: string;
  state: string;
  outcome: string;
  committed: string;
  completed: string;
};

export type OperationRead = {
  data: Operation | undefined;
  error: unknown;
};
