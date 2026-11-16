import {
  Alert,
  Button,
  Card,
  CardGroup,
  LucideIconRefreshCw,
  Skeleton,
} from '@ctrliq/quantic-react';
import { useContext, useEffect, useState } from 'react';

import { isDashboardServer } from '~/domain/auth/utils/predicates';
import { ScreenNotFound } from '~/domain/shell/components/NotFound/ScreenNotFound';
import { EmptyState } from '~/shared/components/EmptyState/EmptyState';
import { PageBleed } from '~/shared/components/PageBleed/PageBleed';
import { PageTitle } from '~/shared/components/PageTitle/PageTitle';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { useServerConfig } from '~/shared/context/ConfigContext';
import { WriteAccessContext } from '~/shared/context/WriteAccessContext';

import {
  allAppsTitle,
  catalogConfigMap,
  catalogNamespace,
  indexKey,
  loadingSubtitle,
  noCatalogSubtitle,
  noCatalogTitle,
  noFilters,
  placeholderCards,
  title,
  unverifiedNotice,
} from '../../constants';
import { useCatalogIndex } from '../../hooks/useCatalogIndex';
import { useCatalogLayout } from '../../hooks/useCatalogLayout';
import { useCatalogWrites } from '../../hooks/useCatalogWrites';
import { useInstalledReleases } from '../../hooks/useInstalledReleases';
import { isInstallable } from '../../schemas/install';
import type { CatalogApp, Confirmation } from '../../types/catalog';
import { essentialsAmong, matchingFilters } from '../../utils/filters';
import { toEmptyMessage, toSubtitle } from '../../utils/formatters';
import { bodyFor, categoriesOf } from '../../utils/selectors';
import { toSeedValues } from '../../utils/transforms';
import styles from './Catalog.module.scss';
import { AppCards, EssentialsStrip } from './parts/AppCards';
import { AppDetail } from './parts/AppDetail';
import { AppsTable } from './parts/AppsTable';
import { CatalogToolbar } from './parts/CatalogToolbar';
import { UninstallDialog } from './parts/UninstallDialog';

const noApps: CatalogApp[] = [];
const placeholders = Array.from({ length: placeholderCards }, (_, index) => index);
const loadingCards = (
  <CardGroup aria-hidden="true">
    {placeholders.map((placeholder) => (
      <Card key={placeholder}>
        <Skeleton rows={4} />
      </Card>
    ))}
  </CardGroup>
);

function CatalogOfCluster() {
  const canWrite = useContext(WriteAccessContext);
  const index = useCatalogIndex();
  const catalog = index.data;
  const apps = catalog?.apps ?? noApps;
  const installed = useInstalledReleases(apps);
  const writes = useCatalogWrites(() => void installed.refetch());
  const { layout, onLayoutChosen } = useCatalogLayout();
  const [filters, setFilters] = useState(noFilters);
  const [selectedId, setSelectedId] = useState<string>();
  const [confirmation, setConfirmation] = useState<Confirmation>();

  useEffect(() => {
    const onKeyPressed = (event: KeyboardEvent): void => {
      if (event.key === 'Escape') {
        setSelectedId(undefined);
      }
    };
    window.addEventListener('keydown', onKeyPressed);
    return () => {
      window.removeEventListener('keydown', onKeyPressed);
    };
  }, []);

  const isLoading = catalog === undefined || index.isFetching;
  const body = bodyFor(catalog, index.isFetching);
  const selected = apps.find(({ id }) => id === selectedId);
  const matching = matchingFilters(apps, filters, installed.releases);
  const essentials = essentialsAmong(apps, filters);
  const listing = {
    releases: installed.releases,
    now: installed.now,
    selectedId,
    canInstall: canWrite && installed.isKnown,
    isBusy: writes.isBusy,
    onOpened: (app: CatalogApp): void => {
      setSelectedId(app.id);
    },
    onQuickInstall: (app: CatalogApp): void => {
      setSelectedId(app.id);
      if (isInstallable(app, toSeedValues(app))) {
        writes.onInstalled(app, toSeedValues(app));
      }
    },
  };
  const onRefreshed = (): void => {
    void index.refetch();
    void installed.refetch();
  };

  return (
    <section className={styles.main} aria-label={title}>
      <header className={styles.heading}>
        <hgroup>
          <PageTitle className={styles.title}>{title}</PageTitle>
          <p className={styles.subtitle}>
            {isLoading
              ? loadingSubtitle
              : catalog.source === 'none'
                ? noCatalogSubtitle
                : toSubtitle(catalog, installed.installedCount)}
          </p>
        </hgroup>
        <Button variant="outline" size="sm" disabled={isLoading} onClick={onRefreshed}>
          <LucideIconRefreshCw size="sm" />
          Refresh
        </Button>
      </header>
      {!isLoading && !installed.isKnown && installed.hasFailed ? (
        <Alert type="error">{unverifiedNotice}</Alert>
      ) : null}
      {body === 'loading' ? (
        loadingCards
      ) : body === 'unavailable' ? (
        <EmptyState
          title={noCatalogTitle}
          actions={
            <Button variant="outline" size="sm" onClick={onRefreshed}>
              Retry
            </Button>
          }
        >
          <p>
            The ConfigMap{' '}
            <code>
              {catalogNamespace}/{catalogConfigMap}
            </code>{' '}
            is missing (or holds no usable <code>{indexKey}</code>), and no reachable catalog URL is
            configured on the fleet manager.
          </p>
          {catalog?.error ? <p>{catalog.error}</p> : null}
        </EmptyState>
      ) : (
        <>
          <div className={styles.content}>
            <div className={styles.listing}>
              {isLoading ? (
                loadingCards
              ) : (
                <>
                  {essentials.length > 0 ? (
                    <EssentialsStrip apps={essentials} {...listing} />
                  ) : null}
                  <CatalogToolbar
                    filters={filters}
                    categories={categoriesOf(apps)}
                    appCount={apps.length}
                    installedCount={installed.installedCount}
                    layout={layout}
                    onFiltersChanged={setFilters}
                    onLayoutChosen={onLayoutChosen}
                  />
                  {essentials.length > 0 ? (
                    <h2 className={styles.listTitle}>{allAppsTitle}</h2>
                  ) : null}
                  {matching.length === 0 ? (
                    <EmptyState
                      actions={
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => {
                            setFilters(noFilters);
                          }}
                        >
                          Clear filters
                        </Button>
                      }
                    >
                      {toEmptyMessage(filters.search)}
                    </EmptyState>
                  ) : layout === 'cards' ? (
                    <AppCards apps={matching} {...listing} />
                  ) : (
                    <PageBleed>
                      <AppsTable apps={matching} {...listing} />
                    </PageBleed>
                  )}
                </>
              )}
            </div>
          </div>
          {selected === undefined ? null : (
            <AppDetail
              key={selected.id}
              app={selected}
              chart={installed.releases.get(selected.id)}
              now={installed.now}
              isStateKnown={installed.isKnown}
              canWrite={canWrite}
              isBusy={writes.isBusy}
              isInstalling={writes.installingId === selected.id}
              onInstalled={writes.onInstalled}
              onUninstallRequested={(app, chart) => {
                setConfirmation({ app, chart });
              }}
              onClosed={() => {
                setSelectedId(undefined);
              }}
            />
          )}
        </>
      )}
      <UninstallDialog
        confirmation={canWrite ? confirmation : undefined}
        onDismissed={() => {
          setConfirmation(undefined);
        }}
        onConfirmed={({ app, chart }) => {
          setConfirmation(undefined);
          writes.onUninstalled(app, chart);
        }}
      />
    </section>
  );
}

export function Catalog() {
  const clusterId = useContext(ClusterContext);
  return isDashboardServer(useServerConfig()) ? (
    <ScreenNotFound />
  ) : (
    <CatalogOfCluster key={clusterId ?? ''} />
  );
}
