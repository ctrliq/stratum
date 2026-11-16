import { essentialsCategory } from '../constants';
import type { CatalogApp, CatalogFilters, Releases } from '../types/catalog';
import { isSearchMatch } from './predicates';

export const matchingFilters = (
  apps: CatalogApp[],
  { search, category, isInstalledOnly }: CatalogFilters,
  releases: Releases,
): CatalogApp[] =>
  apps.filter(
    (app) =>
      (!isInstalledOnly || releases.has(app.id)) &&
      (category === undefined || app.category === category) &&
      isSearchMatch(app, search),
  );

export const essentialsAmong = (
  apps: CatalogApp[],
  { search, category, isInstalledOnly }: CatalogFilters,
): CatalogApp[] =>
  search.trim() !== '' || category !== undefined || isInstalledOnly
    ? []
    : apps.filter((app) => app.category === essentialsCategory);
