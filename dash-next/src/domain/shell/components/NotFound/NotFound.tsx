import { ButtonVariant } from '@ctrliq/quantic-components';
import { Button } from '@ctrliq/quantic-react';

import { EmptyState } from '~/shared/components/EmptyState/EmptyState';

import { useScreenNavigation } from '../../hooks/useScreenNavigation';

type Props = {
  title: string;
  detail: string;
  homeHref: string;
  homeLabel: string;
};

export const NotFound = ({ title, detail, homeHref, homeLabel }: Props) => {
  const { onLinkClicked } = useScreenNavigation();
  return (
    <EmptyState
      title={title}
      actions={
        <Button
          as="a"
          href={homeHref}
          variant={ButtonVariant.Secondary}
          onClick={(event) => {
            onLinkClicked(event, homeHref);
          }}
        >
          {homeLabel}
        </Button>
      }
    >
      {detail}
    </EmptyState>
  );
};
