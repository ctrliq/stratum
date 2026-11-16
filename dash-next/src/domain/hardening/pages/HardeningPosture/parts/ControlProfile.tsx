import { Badge, Panel, PanelHeader } from '@ctrliq/quantic-react';

import type { CisControl } from '~/api/fleet/fleetSchemas';

import { controlListUnavailable, profileCaption, profileFootnote } from '../../../constants';
import { toControlBadge, toGroupHeading, toProfileTitle } from '../../../utils/formatters';
import { toControlGroups } from '../../../utils/transforms';
import styles from './ControlProfile.module.scss';

type Props = {
  profile: CisControl[];
};

const tagColors = { cis: 'blue', exception: 'warning' } as const;

const ControlRow = ({ control }: { control: CisControl }) => {
  const badge = toControlBadge(control.tag);
  return (
    <li className={styles.control}>
      <code>{control.id}</code>
      <span className={styles.description}>
        {control.desc}
        {control.note ? <small>{control.note}</small> : null}
      </span>
      {badge === undefined || control.tag === undefined ? null : (
        <Badge size="sm" color={tagColors[control.tag]}>
          {badge}
        </Badge>
      )}
    </li>
  );
};

export const ControlProfile = ({ profile }: Props) => (
  <Panel>
    <PanelHeader slot="header">
      {toProfileTitle(profile.length)}
      <span slot="subtitle">{profileCaption}</span>
    </PanelHeader>
    {profile.length === 0 ? <p className={styles.footnote}>{controlListUnavailable}</p> : null}
    {toControlGroups(profile).map((group) => (
      <section key={group.group}>
        <h3 className={styles.groupHeading}>{toGroupHeading(group)}</h3>
        <ul className={styles.controls}>
          {group.controls.map((control) => (
            <ControlRow key={control.id} control={control} />
          ))}
        </ul>
      </section>
    ))}
    <p className={styles.footnote}>{profileFootnote}</p>
  </Panel>
);
