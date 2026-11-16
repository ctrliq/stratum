import { useMutation } from '@tanstack/react-query';
import { useContext, useState } from 'react';

import { toast } from '~/shared/components/Toaster/toast';
import { NamespaceContext } from '~/shared/context/NamespaceContext';
import { WriteAccessContext } from '~/shared/context/WriteAccessContext';
import { hasAnswered, toLoadState } from '~/shared/query/loadState';
import type { KubeObject } from '~/shared/types/kubernetes';
import { toErrorNotice } from '~/shared/utils/formatters';
import { fromObjectReference } from '~/shared/utils/parsers';

import { noticeDurationMs } from '../constants';
import type {
  Confirmation,
  Panel,
  ResourceAction,
  ResourceKind,
  ResourceRead,
  ResourceWrites,
} from '../types/resources';
import { offeredAmong, visibleUnder } from '../utils/filters';
import {
  toAppliedNotice,
  toCreatedNotice,
  toDeletedNotice,
  toInvalidScaleNotice,
  toListMessage,
  toRestartedNotice,
  toScaledNotice,
  toStaleNotice,
  toSummary,
} from '../utils/formatters';
import { fromManifestText } from '../utils/parsers';
import { isReplicaCount, offersWrites } from '../utils/predicates';
import { itemsOf, namespaceFor, referencedIn, targetOf } from '../utils/selectors';
import { toDetailPanel, toManifestSkeleton, withShapeChecked } from '../utils/transforms';

type Options<TObject extends KubeObject, TManifest extends KubeObject> = {
  kind: ResourceKind;
  list: ResourceRead<TObject>;
  writes: ResourceWrites<TObject, TManifest>;
  actions: ResourceAction<TObject>[];
  isReadOnly: boolean;
  linkedName: string | undefined;
};

type Sent = {
  send: () => Promise<unknown>;
  notice: string;
  onAccepted?: () => void;
  onSettled?: () => void;
};

const notified = { duration: noticeDurationMs };

export const useResourceBrowser = <
  TObject extends KubeObject,
  TManifest extends KubeObject = TObject,
>({
  kind,
  list: listed,
  writes,
  actions,
  isReadOnly,
  linkedName,
}: Options<TObject, TManifest>) => {
  const namespace = useContext(NamespaceContext);
  const hasWriteControls = offersWrites(useContext(WriteAccessContext), isReadOnly);
  const [search, setSearch] = useState('');
  const [panel, setPanel] = useState<Panel<TObject>>({ type: 'none' });
  const [confirmation, setConfirmation] = useState<Confirmation<TObject>>({ type: 'none' });
  const [openedLink, setOpenedLink] = useState<string>();
  const write = useMutation({ mutationFn: (send: () => Promise<unknown>) => send() });

  const list = withShapeChecked(listed);
  const objects = itemsOf(list);
  const visible = visibleUnder(objects, kind, namespace, search);
  const state = toLoadState(list);
  const linked =
    linkedName === undefined || linkedName === openedLink
      ? undefined
      : referencedIn(objects, fromObjectReference(linkedName));

  if (linked !== undefined) {
    setOpenedLink(linkedName);
    setPanel(toDetailPanel(linked, list.dataUpdatedAt));
  }

  const onPanelClosed = (): void => setPanel({ type: 'none' });
  const onConfirmationDismissed = (): void => setConfirmation({ type: 'none' });

  const onSent = ({ send, notice, onAccepted, onSettled }: Sent): void =>
    write.mutate(send, {
      onSuccess: () => {
        toast.success(notice, notified);
        void list.refetch();
        onAccepted?.();
      },
      onError: (error) => {
        toast.error(toErrorNotice(error), notified);
      },
      onSettled,
    });

  const onApplied = (object: TObject, text: string): void => {
    const parsed = fromManifestText<TManifest>(text);
    if (parsed.isValid) {
      onSent({
        send: () => writes.onReplace(targetOf(object), parsed.manifest),
        notice: toAppliedNotice(kind, object),
        onAccepted: onPanelClosed,
      });
    } else {
      toast.error(parsed.notice, notified);
    }
  };

  const onCreated = (text: string): void => {
    const parsed = fromManifestText<TManifest>(text);
    if (parsed.isValid) {
      onSent({
        send: () =>
          writes.onCreate(
            kind.isNamespaced ? namespaceFor(parsed.manifest, namespace) : '',
            parsed.manifest,
          ),
        notice: toCreatedNotice(kind, parsed.manifest),
        onAccepted: onPanelClosed,
      });
    } else {
      toast.error(parsed.notice, notified);
    }
  };

  const onScaled = (object: TObject, replicas: number | null): void => {
    if (isReplicaCount(replicas)) {
      onSent({
        send: () => writes.scale?.onScale(targetOf(object), replicas) ?? Promise.resolve(),
        notice: toScaledNotice(object, replicas),
        onSettled: onPanelClosed,
      });
    } else {
      toast.error(toInvalidScaleNotice(replicas), notified);
    }
  };

  const onConfirmed = (): void => {
    if (confirmation.type === 'delete') {
      onSent({
        send: () => writes.onDelete(targetOf(confirmation.object)),
        notice: toDeletedNotice(kind, confirmation.object),
      });
    } else if (confirmation.type === 'restart') {
      onSent({
        send: () => writes.onRestart?.(targetOf(confirmation.object)) ?? Promise.resolve(),
        notice: toRestartedNotice(confirmation.object),
      });
    }
    onConfirmationDismissed();
  };

  return {
    namespace,
    hasWriteControls,
    isWriting: write.isPending,
    isLoading: state === 'loading',
    objects: visible,
    summary: toSummary(state, visible.length, objects.length, kind, namespace),
    message: toListMessage(state, hasAnswered(list), visible.length, kind),
    notice: toStaleNotice(state, hasAnswered(list)),
    offeredActions: offeredAmong(actions, hasWriteControls),
    search,
    panel,
    confirmation,
    onSearchChanged: setSearch,
    onOpened: (object: TObject): void => setPanel(toDetailPanel(object, list.dataUpdatedAt)),
    onCreateRequested: (): void =>
      setPanel({ type: 'create', skeleton: toManifestSkeleton(kind, namespace) }),
    onDeleteRequested: (object: TObject): void => {
      onPanelClosed();
      setConfirmation({ type: 'delete', object });
    },
    onRestartRequested: (object: TObject): void => {
      onPanelClosed();
      setConfirmation({ type: 'restart', object });
    },
    onPanelClosed,
    onConfirmationDismissed,
    onConfirmed,
    onApplied,
    onCreated,
    onScaled,
  };
};
