import type {
  CustomResourceColumnDefinition,
  CustomResourceDefinition,
} from '~/api/k8s/k8sSchemas';
import type { ResourceColumns } from '~/domain/resources/types/resources';
import type { ArbitraryKind, ArbitraryObject } from '~/shared/fetchers/arbitraryKinds';

export type DefinitionCells = {
  group: string;
  kind: string;
  scope: string;
  versions: string;
};

export type DefinitionColumns = ResourceColumns<CustomResourceDefinition, DefinitionCells>;

export type PathStep =
  | { type: 'field'; name: string }
  | { type: 'index'; index: number }
  | { type: 'all' }
  | { type: 'filter'; path: string[]; isEqual: boolean; value: string };

export type PrinterColumn = Pick<CustomResourceColumnDefinition, 'name' | 'type'> & {
  id: string;
  description: string | undefined;
  steps: PathStep[] | undefined;
  isNumeric: boolean;
};

export type PrinterCell = {
  text: string;
  fullText?: string;
};

export type InstanceCells = {
  printed: PrinterCell[];
};

export type InstanceColumns = ResourceColumns<ArbitraryObject, InstanceCells>;

export type InstanceAddress = ArbitraryKind;
