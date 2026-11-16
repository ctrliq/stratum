import { Card, CardGroup, CardTitle } from '@ctrliq/quantic-react';
import clsx from 'clsx';

import type { PostureCard } from '../../../types';
import styles from './PostureCards.module.scss';

type Props = {
  cards: PostureCard[];
};

export const PostureCards = ({ cards }: Props) => (
  <CardGroup>
    {cards.map(({ title, value, caption, isWarning }) => (
      <Card key={title}>
        <CardTitle slot="header">
          <span slot="pretitle">{title}</span>
          {value}
        </CardTitle>
        <span className={clsx(styles.caption, { [styles.stateWarning]: isWarning })}>
          {caption}
        </span>
      </Card>
    ))}
  </CardGroup>
);
