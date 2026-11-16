import { fromJsonText } from '~/shared/utils/parsers';
import { isRecord } from '~/shared/utils/predicates';

import {
  defaultCommand,
  execFailureMessage,
  statusChannel,
  stderrChannel,
  stdoutChannel,
} from './constants';
import type { ExecFrame, LogChunk } from './types';

const ignored: ExecFrame = { type: 'ignored' };

export const fromCommand = (command: string | undefined): string[] => {
  const words = (command ?? '').split(' ').filter(Boolean);
  return words.length > 0 ? words : [defaultCommand];
};

export const fromStatusText = (text: string): ExecFrame => {
  const parsed = fromJsonText(text);
  return parsed.isValid && isRecord(parsed.value) && parsed.value.status === 'Failure'
    ? {
        type: 'failure',
        message:
          typeof parsed.value.message === 'string' && parsed.value.message !== ''
            ? parsed.value.message
            : execFailureMessage,
      }
    : ignored;
};

export const fromExecFrame = (data: ArrayBuffer): ExecFrame => {
  const bytes = new Uint8Array(data);
  const channel = bytes[0];
  return channel === stdoutChannel || channel === stderrChannel
    ? { type: 'output', bytes: bytes.subarray(1) }
    : channel === statusChannel
      ? fromStatusText(new TextDecoder().decode(bytes.subarray(1)))
      : ignored;
};

export const fromLogChunk = (pending: string, chunk: string): LogChunk => {
  const parts = `${pending}${chunk}`.split('\n');
  return { lines: parts.slice(0, -1), pending: parts.at(-1) ?? '' };
};
