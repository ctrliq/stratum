import { emptyUsage, usageHistoryLength } from '../constants';
import type { UsageSample, UsageState } from '../types';
import { hasUsageFromEveryNode } from './predicates';
import { toUsageTotals } from './transforms';

const withPercent = (history: number[], percent: number | undefined): number[] =>
  percent === undefined ? history : [...history, percent].slice(-usageHistoryLength);

const withCompleteSample = ({ history }: UsageState, sample: UsageSample): UsageState => {
  const totals = toUsageTotals(sample);
  return {
    totals,
    history: {
      cpu: withPercent(history.cpu, totals.cpuPercent),
      memory: withPercent(history.memory, totals.memoryPercent),
    },
  };
};

export const withUsageSample = (previous: UsageState, sample: UsageSample): UsageState =>
  sample.nodes.length === 0
    ? emptyUsage
    : hasUsageFromEveryNode(sample)
      ? withCompleteSample(previous, sample)
      : previous;
