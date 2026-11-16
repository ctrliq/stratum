import {
  ButtonBar,
  ButtonBarItem,
  LucideIconLayoutGrid,
  LucideIconList,
  LucideIconSearch,
  TextInput,
} from '@ctrliq/quantic-react';

import { allCategories, searchPlaceholder } from '~/domain/catalog/constants';
import type { CatalogFilters, CatalogLayout, CategoryCount } from '~/domain/catalog/types/catalog';
import { toCountText } from '~/domain/catalog/utils/formatters';

import styles from './CatalogToolbar.module.scss';

type Props = {
  filters: CatalogFilters;
  categories: CategoryCount[];
  appCount: number;
  installedCount: number | undefined;
  layout: CatalogLayout;
  onFiltersChanged: (filters: CatalogFilters) => void;
  onLayoutChosen: (layout: CatalogLayout) => void;
};

export const CatalogToolbar = ({
  filters,
  categories,
  appCount,
  installedCount,
  layout,
  onFiltersChanged,
  onLayoutChosen,
}: Props) => (
  <search className={styles.main}>
    <TextInput
      className={styles.search}
      type="search"
      aria-label="Search apps"
      placeholder={searchPlaceholder}
      value={filters.search}
      onValueChanged={(event) => {
        onFiltersChanged({ ...filters, search: event.detail.value });
      }}
    >
      <LucideIconSearch slot="prefix" size="sm" />
    </TextInput>
    <ButtonBar size="sm" aria-label="Category">
      <ButtonBarItem
        active={filters.category === undefined}
        onClick={() => {
          onFiltersChanged({ ...filters, category: undefined });
        }}
      >
        {allCategories.toUpperCase()} {appCount}
      </ButtonBarItem>
      {categories.map(({ category, count }) => (
        <ButtonBarItem
          key={category}
          active={filters.category === category}
          onClick={() => {
            onFiltersChanged({ ...filters, category });
          }}
        >
          {category.toUpperCase()} {count}
        </ButtonBarItem>
      ))}
    </ButtonBar>
    <ButtonBar size="sm" aria-label="Installed filter">
      <ButtonBarItem
        active={filters.isInstalledOnly}
        onClick={() => {
          onFiltersChanged({ ...filters, isInstalledOnly: !filters.isInstalledOnly });
        }}
      >
        Installed {toCountText(installedCount)}
      </ButtonBarItem>
    </ButtonBar>
    <ButtonBar className={styles.layout} size="sm" aria-label="Layout">
      <ButtonBarItem
        aria-label="List view"
        active={layout === 'list'}
        onClick={() => {
          onLayoutChosen('list');
        }}
      >
        <LucideIconList size="sm" />
      </ButtonBarItem>
      <ButtonBarItem
        aria-label="Card view"
        active={layout === 'cards'}
        onClick={() => {
          onLayoutChosen('cards');
        }}
      >
        <LucideIconLayoutGrid size="sm" />
      </ButtonBarItem>
    </ButtonBar>
  </search>
);
