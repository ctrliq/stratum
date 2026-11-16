import { TabButton, TabPanel, Tabs } from '@ctrliq/quantic-react';
import type { ComponentType } from 'react';

import { PageTitle } from '~/shared/components/PageTitle/PageTitle';
import { NestedTitleContext } from '~/shared/context/NestedTitleContext';
import { indexAmong } from '~/shared/utils/selectors';

export type ListTab = {
  id: string;
  label: string;
  List: ComponentType<{ linkedName?: string }>;
};

type Props = {
  title: string;
  tabs: ListTab[];
  tab?: string;
  linkedName?: string;
  onTabChosen: (tab: string) => void;
};

export function TabbedLists({ title, tabs, tab, linkedName, onTabChosen }: Props) {
  const tabIds = tabs.map(({ id }) => id);
  const selected = indexAmong(tabIds, tab);

  return (
    <>
      <PageTitle>{title}</PageTitle>
      <NestedTitleContext.Provider value>
        <Tabs
          size="sm"
          aria-label={title}
          selectedIndex={selected}
          onTabSelected={({ detail }) =>
            detail.index === selected ? undefined : onTabChosen(tabIds[detail.index] ?? '')
          }
        >
          {tabs.map(({ id, label }, index) => (
            <TabButton key={id} index={index}>
              {label}
            </TabButton>
          ))}
          {tabs.map(({ id, List }, index) => (
            <TabPanel key={id} index={index}>
              {index === selected ? <List linkedName={linkedName} /> : null}
            </TabPanel>
          ))}
        </Tabs>
      </NestedTitleContext.Provider>
    </>
  );
}
