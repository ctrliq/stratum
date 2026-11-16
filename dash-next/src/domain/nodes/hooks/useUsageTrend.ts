import { useState } from 'react';

import { emptyUsage } from '~/domain/overview/constants';
import type { UsageSample, UsageState } from '~/domain/overview/types';
import { withUsageSample } from '~/domain/overview/utils/reducers';

type Trend = {
  usage: UsageState;
  sampledAt: number;
};

export const useUsageTrend = (
  sample: UsageSample,
  sampledAt: number,
  isSettled: boolean,
): UsageState => {
  const [trend, setTrend] = useState<Trend>({ usage: emptyUsage, sampledAt: 0 });

  if (isSettled && sampledAt !== trend.sampledAt) {
    setTrend({ usage: withUsageSample(trend.usage, sample), sampledAt });
  }

  return trend.usage;
};
