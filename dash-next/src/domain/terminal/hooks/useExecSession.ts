import { FitAddon } from '@xterm/addon-fit';
import { Terminal } from '@xterm/xterm';
import { useContext, useEffect, useRef, useState } from 'react';

import { ClusterContext } from '~/shared/context/ClusterContext';
import { toExecSocket } from '~/shared/streaming/execSocket';
import { afterClose, afterOpen } from '~/shared/streaming/reducers';
import { toResizeFrame, toStdinFrame } from '~/shared/streaming/transforms';
import type { ExecState, ExecTarget } from '~/shared/streaming/types';

import { connectionFailedLine, sessionClosedLine } from '../constants';
import { toConnectingLine, toDimLine, toFailureLine } from '../utils/formatters';

const fontSize = 13;

const toTerminal = (screen: HTMLElement): Terminal | undefined => {
  try {
    const { backgroundColor, color, fontFamily } = getComputedStyle(screen);
    const terminal = new Terminal({
      cursorBlink: true,
      fontSize,
      fontFamily,
      theme: { background: backgroundColor, foreground: color, cursor: color },
    });
    terminal.open(screen);
    return terminal;
  } catch {
    return undefined;
  }
};

export const useExecSession = ({ namespace, pod, container, command }: ExecTarget) => {
  const clusterId = useContext(ClusterContext);
  const screenRef = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState<ExecState>('connecting');
  const [hasTerminal, setHasTerminal] = useState(true);

  useEffect(() => {
    const target = { namespace, pod, container, command };
    const terminal = screenRef.current ? toTerminal(screenRef.current) : undefined;
    const fit = new FitAddon();
    const session: { state: ExecState } = { state: 'connecting' };
    const onMoved = (state: ExecState): void => {
      session.state = state;
      setStatus(state);
    };
    const socket = terminal
      ? toExecSocket(clusterId, target, {
          onOpened: () => {
            onMoved(afterOpen());
            terminal.focus();
            socket?.send(toResizeFrame(terminal.cols, terminal.rows));
          },
          onFrame: (frame) => {
            if (frame.type === 'output') {
              terminal.write(frame.bytes);
            } else if (frame.type === 'failure') {
              terminal.write(toFailureLine(frame.message));
            }
          },
          onClosed: () => {
            const ended = afterClose(session.state);
            terminal.write(
              toDimLine(ended === 'closed' ? sessionClosedLine : connectionFailedLine),
            );
            onMoved(ended);
          },
        })
      : undefined;
    const observer = new ResizeObserver(() => {
      fit.fit();
    });

    if (terminal && screenRef.current) {
      terminal.loadAddon(fit);
      terminal.write(toConnectingLine(target));
      terminal.onData((data) => socket?.send(toStdinFrame(data)));
      terminal.onResize(({ cols, rows }) => socket?.send(toResizeFrame(cols, rows)));
      observer.observe(screenRef.current);
    } else {
      onMoved('error');
      setHasTerminal(false);
    }

    return () => {
      observer.disconnect();
      socket?.close();
      terminal?.dispose();
    };
  }, [clusterId, namespace, pod, container, command]);

  return { screenRef, status, hasTerminal };
};
