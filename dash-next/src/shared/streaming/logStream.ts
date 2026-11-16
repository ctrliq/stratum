import { currentBackend } from '~/shared/fetchers/backend';
import { k8sBaseFor, requestDeadlineMs, toUrl } from '~/shared/fetchers/requests';
import { toErrorMessage } from '~/shared/utils/formatters';

import { deadlineMessage } from './constants';
import { toRefusalText } from './formatters';
import { fromLogChunk } from './parsers';
import { isBoundedAfterAnswer } from './predicates';
import { toLogAddress } from './transforms';
import type { LogRequest } from './types';

export type LogStreamHandlers = {
  onLines: (lines: string[]) => void;
  onEnded: () => void;
  onFailed: (message: string) => void;
};

const unauthorized = 401;

const readToEnd = async (
  body: Response['body'],
  onLines: LogStreamHandlers['onLines'],
  onChunk: () => void,
): Promise<void> => {
  const reader = body?.pipeThrough(new TextDecoderStream()).getReader();
  let pending = '';
  let chunk = await reader?.read();
  while (chunk && !chunk.done) {
    onChunk();
    const parsed = fromLogChunk(pending, chunk.value);
    pending = parsed.pending;
    if (parsed.lines.length > 0) {
      onLines(parsed.lines);
    }
    chunk = await reader?.read();
  }
  if (pending !== '') {
    onLines([pending]);
  }
};

export const streamedLog = async (
  clusterId: string | undefined,
  request: LogRequest,
  { onLines, onEnded, onFailed }: LogStreamHandlers,
  signal: AbortSignal,
): Promise<void> => {
  const url = toUrl({
    ...toLogAddress(request),
    method: 'get',
    baseUrl: k8sBaseFor(currentBackend().k8sBase, clusterId),
  });
  const answering = new AbortController();
  const toDeadline = (): ReturnType<typeof setTimeout> =>
    setTimeout(() => {
      answering.abort();
    }, requestDeadlineMs);
  let deadline = toDeadline();
  const onChunk = (): void => {
    clearTimeout(deadline);
    deadline = isBoundedAfterAnswer(request) ? toDeadline() : deadline;
  };
  try {
    const response = await fetch(url, {
      credentials: 'same-origin',
      signal: AbortSignal.any([signal, answering.signal]),
    });
    onChunk();
    if (response.ok && response.body !== null) {
      await readToEnd(response.body, onLines, onChunk);
      clearTimeout(deadline);
      onEnded();
    } else {
      const body = await response.text();
      clearTimeout(deadline);
      if (response.status === unauthorized) {
        currentBackend().onUnauthorized(url);
      }
      onFailed(toRefusalText(response.status, body));
    }
  } catch (error) {
    clearTimeout(deadline);
    if (!signal.aborted) {
      onFailed(answering.signal.aborted ? deadlineMessage : toErrorMessage(error));
    }
  }
};
