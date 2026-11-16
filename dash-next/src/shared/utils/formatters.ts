import { Document, isScalar, Scalar, visit } from 'yaml';

import { type ApiError, isApiError } from '~/shared/fetchers/requests';
import type { Tone } from '~/shared/types/tone';

import { isRecord } from './predicates';

export const absentText = '—';

const secondsPerMinute = 60;
const minutesPerHour = 60;
const hoursPerDay = 24;
const lastSecondsReading = 90;
const lastMinutesReading = 90;
const lastHoursReading = 48;
const nanocoresPerMillicore = 1e6;
const bytesPerMebibyte = 1024 ** 2;

const statusLabels: Record<string, string> = {
  SchedulingDisabled: 'Cordoned',
  CrashLoopBackOff: 'CrashLoop',
  healthy: 'Healthy',
  degraded: 'Degraded',
  disconnected: 'Disconnected',
  pending: 'Pending',
  provisioning: 'Provisioning',
  deployed: 'Deployed',
};

const statusesByTone = {
  success: [
    'Running',
    'Ready',
    'Healthy',
    'Complete',
    'Completed',
    'Deployed',
    'Active',
    'Bound',
    'Done',
    'healthy',
    'deployed',
  ],
  progress: ['Progressing', 'Validating', 'Upgrading', 'pending', 'provisioning'],
  warning: ['Degraded', 'Paused', 'SchedulingDisabled', 'Terminating', 'Unschedulable', 'degraded'],
  error: ['Failed', 'NotReady', 'CrashLoopBackOff', 'disconnected'],
};

const usageWarningAbove = 70;
const usageErrorAbove = 85;

const toElapsedText = (seconds: number): string => {
  const minutes = seconds / secondsPerMinute;
  const hours = minutes / minutesPerHour;
  return seconds < lastSecondsReading
    ? `${Math.round(seconds)}s`
    : minutes < lastMinutesReading
      ? `${Math.round(minutes)}m`
      : hours < lastHoursReading
        ? `${Math.round(hours)}h`
        : `${Math.round(hours / hoursPerDay)}d`;
};

export const toAge = (timestamp: string | undefined, now: number): string => {
  const createdAt = Date.parse(timestamp ?? '');
  return Number.isNaN(createdAt) ? absentText : toElapsedText(Math.max(0, now - createdAt) / 1000);
};

export const toDisplayText = (value: string | number | undefined | null): string =>
  value === undefined || value === null || value === '' ? absentText : String(value);

export const toMillicores = (nanocores: number): number => nanocores / nanocoresPerMillicore;

export const toMebibytes = (bytes: number): number => bytes / bytesPerMebibyte;

export const toJson = (value: unknown): string => JSON.stringify(value, null, 2);

export const toYaml = (value: unknown): string => {
  const document = new Document(value);
  visit(document, {
    Scalar: (_key, node) => {
      if (isScalar(node) && node.value === '') {
        node.type = Scalar.QUOTE_SINGLE;
      }
    },
  });
  return document.toString({ blockQuote: false, lineWidth: 0 }).trimEnd();
};

export const toLineCount = (text: string): string => {
  const count = text.split('\n').length;
  return count === 1 ? '1 line' : `${count} lines`;
};

const toRefusalMessage = ({ status, payload }: ApiError): string =>
  isRecord(payload) && typeof payload.message === 'string'
    ? payload.message
    : isRecord(payload) && typeof payload.error === 'string'
      ? payload.error
      : typeof payload === 'string' && payload.trim() !== ''
        ? payload.trim()
        : `HTTP ${status}`;

export const toErrorMessage = (error: unknown): string =>
  isApiError(error)
    ? toRefusalMessage(error)
    : error instanceof Error
      ? error.message
      : String(error);

export const toListText = (values: string[]): string =>
  values.length > 0 ? values.join(', ') : absentText;

export const toLocalTime = (timestamp: string | undefined | null): string => {
  const instant = new Date(timestamp ?? '');
  return !timestamp
    ? absentText
    : Number.isNaN(instant.getTime())
      ? timestamp
      : instant.toLocaleString();
};

export const toAgeAgo = (timestamp: string | undefined | null, now: number): string | undefined => {
  const seconds = Math.round((now - Date.parse(timestamp ?? '')) / 1000);
  const minutes = Math.floor(seconds / secondsPerMinute);
  const hours = Math.floor(minutes / minutesPerHour);
  return Number.isNaN(seconds) || seconds < 0
    ? undefined
    : seconds < secondsPerMinute
      ? `${seconds}s ago`
      : minutes < minutesPerHour
        ? `${minutes}m ago`
        : hours < hoursPerDay
          ? `${hours}h ago`
          : `${Math.floor(hours / hoursPerDay)}d ago`;
};

export const toErrorNotice = (error: unknown): string => `Error: ${toErrorMessage(error)}`;

export const toStatusLabel = (status: string): string => statusLabels[status] ?? status;

export const toStatusTone = (status: string): Tone =>
  statusesByTone.success.includes(status)
    ? 'success'
    : statusesByTone.progress.includes(status)
      ? 'progress'
      : statusesByTone.warning.includes(status)
        ? 'warning'
        : statusesByTone.error.includes(status)
          ? 'error'
          : 'neutral';

export const toUsageTone = (percent: number | undefined): Tone | undefined =>
  !percent
    ? undefined
    : percent > usageErrorAbove
      ? 'error'
      : percent > usageWarningAbove
        ? 'warning'
        : 'success';

export const toPercentText = (percent: number): string => `${percent}%`;

const toSearchPairs = ([key, value]: [string, unknown]): [string, string][] =>
  value === undefined || value === null
    ? []
    : Array.isArray(value)
      ? value.map((item): [string, string] => [key, String(item)])
      : [[key, String(value)]];

export const toSearchString = (search: Record<string, unknown>): string => {
  const query = new URLSearchParams(Object.entries(search).flatMap(toSearchPairs)).toString();
  return query ? `?${query}` : '';
};
