import { ButtonBar, ButtonBarItem } from '@ctrliq/quantic-react';

import { allKindsLabel, kindFilterName, overviewKinds } from '../../../constants';
import type { WorkloadKind } from '../../../types/workloads';
import { toKindLabel } from '../../../utils/formatters';

type Props = {
  chosen: WorkloadKind | undefined;
  counts: Partial<Record<string, number>>;
  total: number;
  onChosen: (kind: WorkloadKind | undefined) => void;
};

export function KindFilter({ chosen, counts, total, onChosen }: Props) {
  return (
    <ButtonBar size="sm" aria-label={kindFilterName}>
      <ButtonBarItem active={chosen === undefined} onClick={() => onChosen(undefined)}>
        {toKindLabel(allKindsLabel, total)}
      </ButtonBarItem>
      {overviewKinds.map((kind) => (
        <ButtonBarItem key={kind} active={kind === chosen} onClick={() => onChosen(kind)}>
          {toKindLabel(kind, counts[kind])}
        </ButtonBarItem>
      ))}
    </ButtonBar>
  );
}
