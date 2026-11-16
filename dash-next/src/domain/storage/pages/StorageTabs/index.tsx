import { type ListTab, TabbedLists } from '~/shared/components/TabbedLists/TabbedLists';

import { claimKind, storageClassKind, volumeKind } from '../../constants';
import { PersistentVolumeClaimsList } from '../PersistentVolumeClaimsList';
import { PersistentVolumesList } from '../PersistentVolumesList';
import { StorageClassesList } from '../StorageClassesList';

type Props = {
  tab?: string;
  linkedName?: string;
  onTabChosen: (tab: string) => void;
};

const tabs: ListTab[] = [
  { id: 'persistentvolumeclaims', label: claimKind.plural, List: PersistentVolumeClaimsList },
  { id: 'persistentvolumes', label: volumeKind.plural, List: PersistentVolumesList },
  { id: 'storageclasses', label: storageClassKind.plural, List: StorageClassesList },
];

export function StorageTabs(props: Props) {
  return <TabbedLists title="Storage" tabs={tabs} {...props} />;
}
