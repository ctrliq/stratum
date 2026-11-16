import { toStatusLabel, toStatusTone } from '~/shared/utils/formatters';

import { ToneBadge } from './ToneBadge';

type Props = {
  getValue: () => unknown;
};

export function StatusCell({ getValue }: Props) {
  const status = String(getValue());
  return <ToneBadge tone={toStatusTone(status)}>{toStatusLabel(status)}</ToneBadge>;
}
