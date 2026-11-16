import { Button, Checkbox, Select, TextInput } from '@ctrliq/quantic-react';
import { useEffect, useRef, useState } from 'react';

import { noLogOutput } from '../../constants';
import { useLogViewer } from '../../hooks/useLogViewer';
import type { PodTarget } from '../../types/terminal';
import { matchingFilter } from '../../utils/filters';
import { toLineCounter } from '../../utils/formatters';
import styles from './LogViewer.module.scss';

type Props = {
  pod: PodTarget;
};

export function LogViewer({ pod }: Props) {
  const { view, onOptionChanged, onStopped, onRestarted } = useLogViewer(pod);
  const [filter, setFilter] = useState('');
  const [isAutoScrolling, setIsAutoScrolling] = useState(true);
  const outputRef = useRef<HTMLPreElement>(null);
  const shown = matchingFilter(view.lines, filter);
  const text = shown.join('\n');

  useEffect(() => {
    if (isAutoScrolling && outputRef.current) {
      outputRef.current.scrollTop = outputRef.current.scrollHeight;
    }
  }, [text, isAutoScrolling]);

  return (
    <section className={styles.main} aria-label={`Logs of ${pod.name}`}>
      <header className={styles.toolbar}>
        {pod.containers.length > 1 ? (
          <Select
            size="sm"
            aria-label="Container"
            options={pod.containers.map((container) => [container, container])}
            value={view.container ?? ''}
            onValueChanged={(event) => {
              onOptionChanged({ container: event.detail.value });
            }}
          />
        ) : null}
        <TextInput
          className={styles.filter}
          size="sm"
          type="search"
          aria-label="Filter log lines"
          placeholder="error"
          value={filter}
          onValueChanged={(event) => {
            setFilter(event.detail.value);
          }}
        />
        <Checkbox
          size="sm"
          label="Follow"
          checked={view.isFollowing}
          onValueChanged={(event) => {
            onOptionChanged({ isFollowing: event.detail.checked });
          }}
        />
        <Checkbox
          size="sm"
          label="Timestamps"
          checked={view.hasTimestamps}
          onValueChanged={(event) => {
            onOptionChanged({ hasTimestamps: event.detail.checked });
          }}
        />
        <Checkbox
          size="sm"
          label="Previous"
          checked={view.isPrevious}
          onValueChanged={(event) => {
            onOptionChanged({ isPrevious: event.detail.checked });
          }}
        />
        <Checkbox
          size="sm"
          label="Auto-scroll"
          checked={isAutoScrolling}
          onValueChanged={(event) => {
            setIsAutoScrolling(event.detail.checked);
          }}
        />
        <output className={styles.counter}>{toLineCounter(shown.length)}</output>
        <Button variant="outline" size="sm" onClick={view.isStreaming ? onStopped : onRestarted}>
          {view.isStreaming ? 'Stop' : 'Stream'}
        </Button>
      </header>
      <pre ref={outputRef} className={styles.output}>
        {text}
      </pre>
      {!view.isStreaming && shown.length === 0 ? (
        <p className={styles.empty}>{noLogOutput}</p>
      ) : null}
    </section>
  );
}
