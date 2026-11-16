import { useContext, useEffect, useState } from 'react';

import { ClusterContext } from '~/shared/context/ClusterContext';
import { type LogStreamHandlers, streamedLog } from '~/shared/streaming/logStream';

import type { LogOptions, PodTarget } from '../types/terminal';
import {
  asRestarted,
  asStopped,
  toLogView,
  withArrived,
  withFailure,
  withOption,
} from '../utils/reducers';

export const useLogViewer = ({ namespace, name, containers }: PodTarget) => {
  const clusterId = useContext(ClusterContext);
  const [view, setView] = useState(() => toLogView(containers));
  const { run, isStreaming, container, isFollowing, hasTimestamps, isPrevious } = view;

  useEffect(() => {
    const stopping = new AbortController();
    const request = { namespace, pod: name, container, isFollowing, hasTimestamps, isPrevious };
    const handlers: LogStreamHandlers = {
      onLines: (lines) => {
        setView((current) => withArrived(current, lines));
      },
      onEnded: () => {
        setView(asStopped);
      },
      onFailed: (message) => {
        setView((current) => withFailure(current, message));
      },
    };
    if (isStreaming) {
      void streamedLog(clusterId, request, handlers, stopping.signal);
    }
    return () => {
      stopping.abort();
    };
  }, [
    run,
    isStreaming,
    clusterId,
    namespace,
    name,
    container,
    isFollowing,
    hasTimestamps,
    isPrevious,
  ]);

  return {
    view,
    onOptionChanged: (option: Partial<LogOptions>): void => {
      setView((current) => withOption(current, option));
    },
    onStopped: (): void => {
      setView(asStopped);
    },
    onRestarted: (): void => {
      setView(asRestarted);
    },
  };
};
