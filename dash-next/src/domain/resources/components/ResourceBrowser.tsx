import { Button, ConfirmDialog } from '@ctrliq/quantic-react';
import { type ReactNode, useContext } from 'react';

import { ResourceList } from '~/shared/components/ResourceList/ResourceList';
import { ClusterContext } from '~/shared/context/ClusterContext';
import type { KubeObject } from '~/shared/types/kubernetes';

import { useResourceBrowser } from '../hooks/useResourceBrowser';
import type {
  OpenedQuery,
  ResourceAction,
  ResourceColumns,
  ResourceKind,
  ResourceRead,
  ResourceRow,
  ResourceWrites,
} from '../types/resources';
import {
  toDeleteHeading,
  toDeleteWarning,
  toOpenPrefix,
  toRestartHeading,
  toRestartWarning,
  toSearchName,
  toSearchPlaceholder,
} from '../utils/formatters';
import { isShownIn } from '../utils/predicates';
import { targetOf } from '../utils/selectors';
import { toResourceRow, withIdentityColumns } from '../utils/transforms';
import { LoadedDetail } from './LoadedDetail';
import { ResourceCreate } from './ResourceCreate';
import { ResourceDetail } from './ResourceDetail';

type Props<TObject extends KubeObject, TCells, TOpened extends KubeObject> = {
  kind: ResourceKind;
  list: ResourceRead<TObject>;
  columns: ResourceColumns<TObject, TCells>;
  toRow: (object: TObject) => TCells;
  writes: ResourceWrites<TObject, TOpened>;
  actions?: ResourceAction<TObject>[];
  isReadOnly?: boolean;
  linkedName?: string;
  openedQuery?: OpenedQuery<TOpened>;
  tags?: ReactNode;
  hasIdentityColumns?: boolean;
};

const noActions: never[] = [];

function ResourceBrowserIn<TObject extends KubeObject, TCells, TOpened extends KubeObject>({
  kind,
  list,
  columns,
  toRow,
  writes,
  actions = noActions,
  isReadOnly = false,
  linkedName,
  openedQuery,
  tags,
  hasIdentityColumns = true,
}: Props<TObject, TCells, TOpened>) {
  const browser = useResourceBrowser({ kind, list, writes, actions, isReadOnly, linkedName });
  const { panel, confirmation } = browser;
  const detail =
    panel.type === 'detail'
      ? {
          kind,
          age: panel.age,
          actions: browser.offeredActions.map(({ title, onRun }) => ({
            title,
            onRun: () => onRun(panel.object),
          })),
          isScalable: writes.scale !== undefined,
          replicas: writes.scale?.replicasOf(panel.object),
          hasWriteControls: browser.hasWriteControls,
          isRestartable: writes.onRestart !== undefined,
          isWriting: browser.isWriting,
          onApplied: (text: string) => browser.onApplied(panel.object, text),
          onScaled: (replicas: number | null) => browser.onScaled(panel.object, replicas),
          onDeleteRequested: () => browser.onDeleteRequested(panel.object),
          onRestartRequested: () => browser.onRestartRequested(panel.object),
          onClosed: browser.onPanelClosed,
        }
      : undefined;
  const detailKey =
    panel.type === 'detail'
      ? `${panel.object.metadata?.uid}/${panel.object.metadata?.namespace}/${panel.object.metadata?.name}`
      : '';

  return (
    <>
      <ResourceList<ResourceRow<TObject, TCells>>
        title={kind.plural}
        tags={tags}
        summary={browser.summary}
        rows={browser.objects.map(toResourceRow(toRow, list.dataUpdatedAt))}
        columns={hasIdentityColumns ? withIdentityColumns(kind, columns) : columns}
        nameOf={(row) => row.name}
        openPrefix={toOpenPrefix(kind)}
        isRowSelected={(row) => isShownIn(row.object, panel)}
        onRowOpened={(row) => browser.onOpened(row.object)}
        rowActions={browser.offeredActions.map(({ title, icon, onRun }) => ({
          title,
          icon,
          onRun: (row) => onRun(row.object),
        }))}
        isLoading={browser.isLoading}
        message={browser.message}
        notice={browser.notice}
        search={browser.search}
        searchName={toSearchName(kind)}
        searchPlaceholder={toSearchPlaceholder(kind)}
        onSearchChanged={browser.onSearchChanged}
        primaryAction={
          browser.hasWriteControls ? (
            <Button onClick={browser.onCreateRequested}>Create</Button>
          ) : null
        }
        panel={
          panel.type === 'detail' && detail ? (
            openedQuery ? (
              <LoadedDetail
                key={detailKey}
                listed={panel.object}
                query={openedQuery(targetOf(panel.object))}
                {...detail}
              />
            ) : (
              <ResourceDetail key={detailKey} object={panel.object} {...detail} />
            )
          ) : panel.type === 'create' ? (
            <ResourceCreate
              kind={kind}
              skeleton={panel.skeleton}
              isWriting={browser.isWriting}
              onCreated={browser.onCreated}
              onClosed={browser.onPanelClosed}
            />
          ) : null
        }
      />
      <ConfirmDialog
        open={confirmation.type === 'delete'}
        heading={toDeleteHeading(kind)}
        message={confirmation.type === 'delete' ? toDeleteWarning(confirmation.object) : ''}
        confirmText="Delete"
        destructive
        loading={browser.isWriting}
        onConfirm={browser.onConfirmed}
        onCancel={browser.onConfirmationDismissed}
      />
      <ConfirmDialog
        open={confirmation.type === 'restart'}
        heading={toRestartHeading(kind)}
        message={confirmation.type === 'restart' ? toRestartWarning(confirmation.object) : ''}
        confirmText="Restart"
        loading={browser.isWriting}
        onConfirm={browser.onConfirmed}
        onCancel={browser.onConfirmationDismissed}
      />
    </>
  );
}

export function ResourceBrowser<
  TObject extends KubeObject,
  TCells,
  TOpened extends KubeObject = TObject,
>(props: Props<TObject, TCells, TOpened>) {
  const clusterId = useContext(ClusterContext);
  return <ResourceBrowserIn key={clusterId ?? ''} {...props} />;
}
