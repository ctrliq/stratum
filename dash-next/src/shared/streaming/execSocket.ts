import { currentBackend } from '~/shared/fetchers/backend';
import { k8sBaseFor } from '~/shared/fetchers/requests';

import { execSubprotocol } from './constants';
import { fromExecFrame } from './parsers';
import { toExecPath, toSocketUrl } from './transforms';
import type { ExecFrame, ExecTarget } from './types';

export type ExecSocketHandlers = {
  onOpened: () => void;
  onFrame: (frame: ExecFrame) => void;
  onClosed: () => void;
};

export type ExecSocket = {
  send: (frame: Uint8Array) => void;
  close: () => void;
};

export const toExecSocket = (
  clusterId: string | undefined,
  target: ExecTarget,
  { onOpened, onFrame, onClosed }: ExecSocketHandlers,
): ExecSocket => {
  const path = toExecPath(k8sBaseFor(currentBackend().k8sBase, clusterId), target);
  const socket = new WebSocket(toSocketUrl(window.location, path), execSubprotocol);
  const listening = new AbortController();
  const { signal } = listening;
  socket.binaryType = 'arraybuffer';
  socket.addEventListener('open', onOpened, { signal });
  socket.addEventListener(
    'message',
    ({ data }: MessageEvent<unknown>) => {
      if (data instanceof ArrayBuffer) {
        onFrame(fromExecFrame(data));
      }
    },
    { signal },
  );
  socket.addEventListener(
    'close',
    ({ wasClean: isSessionStillTrusted }) => {
      onClosed();
      if (!isSessionStillTrusted) {
        currentBackend().onUnauthorized(path);
      }
    },
    { signal },
  );
  return {
    send: (frame) => {
      if (socket.readyState === WebSocket.OPEN) {
        socket.send(frame);
      }
    },
    close: () => {
      listening.abort();
      socket.close();
    },
  };
};
