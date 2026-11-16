import { ButtonGrow, ButtonVariant, IconSize } from '@ctrliq/quantic-components';
import { Button, LucideIconCheck, TextInput } from '@ctrliq/quantic-react';
import clsx from 'clsx';
import { useState } from 'react';

import { useServerConfig } from '~/shared/context/ConfigContext';

import { useScreenNavigation } from '../../hooks/useScreenNavigation';
import { useScreenPlace } from '../../hooks/useScreenPlace';
import type { Fleet, PaletteAction, PaletteEntry } from '../../types/screen';
import { matchingQuery } from '../../utils/filters';
import { freshPalette, withHighlight, withHighlightMoved, withQuery } from '../../utils/reducers';
import { toPaletteEntries, toPaletteGroups } from '../../utils/transforms';
import styles from './PaletteEntries.module.scss';

type Props = {
  fleet: Fleet;
  actions: PaletteAction[];
  onClosed: () => void;
};

const steps: Record<string, number> = { ArrowDown: 1, ArrowUp: -1 };

export const PaletteEntries = ({ fleet, actions, onClosed }: Props) => {
  const config = useServerConfig();
  const { clusterId, namespace } = useScreenPlace();
  const { onOpened } = useScreenNavigation();
  const [palette, setPalette] = useState(freshPalette);
  const entries = matchingQuery(
    toPaletteEntries({ config, clusterId, namespace }, fleet, actions),
    palette.query,
  );

  const onRun = (entry: PaletteEntry | undefined): void => {
    if (entry) {
      onClosed();
      if ('href' in entry) {
        onOpened(entry.href);
      } else {
        entry.onRun();
      }
    }
  };

  const onKeyPressed = (event: Event): void => {
    const key = event instanceof KeyboardEvent ? event.key : '';
    const step = steps[key];
    if (step !== undefined) {
      event.preventDefault();
      setPalette((current) => withHighlightMoved(current, step, entries.length));
    } else if (key === 'Enter') {
      onRun(entries[palette.highlighted]);
    }
  };

  return (
    <search className={styles.palette}>
      <TextInput
        aria-label="Search screens and clusters"
        placeholder="pods"
        value={palette.query}
        onValueChanged={(event) => {
          setPalette((current) => withQuery(current, event.detail.value));
        }}
        onKeydown={onKeyPressed}
      />
      {toPaletteGroups(entries).map((group) => (
        <section key={group.group} aria-label={group.group}>
          <h2 className={styles.groupName}>{group.group}</h2>
          <ul className={styles.entries}>
            {group.entries.map((entry) => {
              const position = entries.indexOf(entry);
              return (
                <li
                  key={entry.id}
                  className={clsx(styles.entry, {
                    [styles.stateHighlighted]: position === palette.highlighted,
                  })}
                >
                  <Button
                    variant={ButtonVariant.Ghost}
                    grow={ButtonGrow.Always}
                    onPointerEnter={() => {
                      setPalette((current) => withHighlight(current, position));
                    }}
                    onClick={() => {
                      onRun(entry);
                    }}
                  >
                    <span className={styles.title}>{entry.title}</span>
                    {entry.subtitle ? (
                      <small className={styles.subtitle}>{entry.subtitle}</small>
                    ) : null}
                    {entry.isCurrent ? (
                      <LucideIconCheck size={IconSize.Small} aria-label="Current cluster" />
                    ) : null}
                  </Button>
                </li>
              );
            })}
          </ul>
        </section>
      ))}
      {entries.length === 0 ? (
        <p className={styles.noMatches}>No matches for &quot;{palette.query}&quot;</p>
      ) : null}
      <footer className={styles.keys}>↑↓ to navigate · ↵ to select · esc to close</footer>
    </search>
  );
};
