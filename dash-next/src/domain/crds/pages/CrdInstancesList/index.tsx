import { Button, ButtonBar, ButtonBarItem, Tag, TagGroup } from '@ctrliq/quantic-react';
import { useQuery } from '@tanstack/react-query';
import { useContext } from 'react';

import { readApiextensionsV1CustomResourceDefinitionQuery } from '~/api/k8s/k8sComponents';
import { useK8sContext } from '~/api/k8s/k8sContext';
import { useScreenNavigation } from '~/domain/shell/hooks/useScreenNavigation';
import { ClusterContext } from '~/shared/context/ClusterContext';
import { NamespaceContext } from '~/shared/context/NamespaceContext';

import { backToDefinitions, versionSwitchName } from '../../constants';
import type { InstanceAddress } from '../../types/crds';
import {
  toDefinitionName,
  toDefinitionsHref,
  toInstancesHref,
  toInstanceTags,
  toResolvingText,
  toUnreadableNote,
} from '../../utils/formatters';
import { servedVersionsOf } from '../../utils/selectors';
import styles from './CrdInstancesList.module.scss';
import { InstanceBrowser } from './parts/InstanceBrowser';

type Props = InstanceAddress & {
  linkedName?: string;
};

const readOnce = {
  staleTime: Infinity,
  gcTime: 0,
  retry: false,
  refetchOnWindowFocus: false,
  refetchOnReconnect: false,
};

export function CrdInstancesList({ group, version, plural, linkedName }: Props) {
  const clusterId = useContext(ClusterContext) ?? '';
  const namespace = useContext(NamespaceContext);
  const { onOpened } = useScreenNavigation();
  const address = { group, version, plural };
  const { fetcherOptions } = useK8sContext();
  const definitionRead = readApiextensionsV1CustomResourceDefinitionQuery({
    ...fetcherOptions,
    pathParams: { name: toDefinitionName(address) },
  });
  const { data: definition, isPending } = useQuery({
    ...definitionRead,
    queryKey: [...definitionRead.queryKey, version],
    ...readOnce,
  });
  const versions = servedVersionsOf(definition);

  return (
    <section className={styles.main} aria-label={toDefinitionName(address)}>
      <header className={styles.bar}>
        <Button
          variant="ghost"
          size="sm"
          onClick={() => {
            onOpened(toDefinitionsHref(clusterId, namespace));
          }}
        >
          {backToDefinitions}
        </Button>
        {versions.length > 1 ? (
          <ButtonBar size="sm" aria-label={versionSwitchName}>
            {versions.map((served) => (
              <ButtonBarItem
                key={served}
                active={served === version}
                onClick={() => {
                  onOpened(toInstancesHref(clusterId, { ...address, version: served }, namespace));
                }}
              >
                {served}
              </ButtonBarItem>
            ))}
          </ButtonBar>
        ) : null}
        {isPending || definition ? null : (
          <p className={styles.note}>{toUnreadableNote(address)}</p>
        )}
      </header>
      {isPending ? (
        <p role="status" className={styles.resolving}>
          {toResolvingText(address)}
        </p>
      ) : (
        <div className={styles.list}>
          <InstanceBrowser
            key={`${group}/${version}/${plural}`}
            definition={definition}
            address={address}
            linkedName={linkedName}
            tags={
              <TagGroup aria-label="API facts">
                {toInstanceTags(definition, address).map((tag) => (
                  <Tag key={tag} size="sm">
                    {tag}
                  </Tag>
                ))}
              </TagGroup>
            }
          />
        </div>
      )}
    </section>
  );
}
