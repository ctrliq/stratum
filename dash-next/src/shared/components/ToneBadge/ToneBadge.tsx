import { BadgeColor, BadgeSize } from '@ctrliq/quantic-components';
import { StatusBadge } from '@ctrliq/quantic-react';
import type { ReactNode } from 'react';

import { Hinted } from '~/shared/components/Hinted/Hinted';
import type { Tone } from '~/shared/types/tone';

type Props = {
  tone: Tone;
  hint?: string;
  slot?: string;
  children: ReactNode;
};

const colors: Record<Tone, BadgeColor> = {
  success: BadgeColor.Success,
  progress: BadgeColor.Blue,
  neutral: BadgeColor.Gray,
  warning: BadgeColor.Warning,
  error: BadgeColor.Error,
};

export function ToneBadge({ tone, hint, slot, children }: Props) {
  return (
    <Hinted hint={hint}>
      <StatusBadge color={colors[tone]} size={BadgeSize.Small} slot={slot}>
        {children}
      </StatusBadge>
    </Hinted>
  );
}
